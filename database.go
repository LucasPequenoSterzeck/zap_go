package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const (
	queryTimeout = 30 * time.Second
)

type Message struct {
	GeradaPor string
	GeradaEm  time.Time
	Meio      string
	Para      string
	Destino   string
	Mensagem  string
	Anexo     sql.NullString
	Status    int
}

type Database struct {
	db *sql.DB
}

func NewDatabase(dbPath string) (*Database, error) {
	// Abre a conexão com o banco de dados
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("erro ao abrir banco de dados: %v", err)
	}

	// Configura as otimizações do SQLite
	_, err = db.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA synchronous = NORMAL;
		PRAGMA temp_store = MEMORY;
		PRAGMA cache_size = -2000;
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("erro ao configurar banco de dados: %v", err)
	}

	// Cria a tabela se não existir
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS msg (
			gerada_por TEXT,
			gerada_em TEXT,
			meio TEXT,
			para TEXT,
			destino TEXT,
			mensagem TEXT,
			anexo TEXT,
			status INTEGER DEFAULT 0
		)
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("erro ao criar tabela: %v", err)
	}

	return &Database{db: db}, nil
}

func (d *Database) Close() error {
	return d.db.Close()
}

// GetPendingMessages retorna mensagens pendentes de envio
func (d *Database) GetPendingMessages(ctx context.Context) ([]Message, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	query := `
		SELECT gerada_por, gerada_em, meio, para, destino, mensagem, anexo 
		FROM msg 
		WHERE status = 0 AND meio = 'WhatsApp'
		LIMIT 50
	`

	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("erro ao buscar mensagens pendentes: %v", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var geradaEmStr string
		err := rows.Scan(
			&msg.GeradaPor,
			&geradaEmStr,
			&msg.Meio,
			&msg.Para,
			&msg.Destino,
			&msg.Mensagem,
			&msg.Anexo,
		)
		if err != nil {
			return nil, fmt.Errorf("erro ao ler mensagem: %v", err)
		}

		// Converte a string de data para time.Time
		msg.GeradaEm, err = time.Parse("2006-01-02 15:04:05", geradaEmStr)
		if err != nil {
			// Se falhar no formato padrão, tenta com o formato ISO
			msg.GeradaEm, err = time.Parse("2006-01-02 15:04:05.000Z", geradaEmStr+"Z")
			if err != nil {
				return nil, fmt.Errorf("erro ao converter data '%s': %v", geradaEmStr, err)
			}
		}

		msg.Status = 0
		messages = append(messages, msg)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("erro ao iterar mensagens: %v", err)
	}

	return messages, nil
}

// UpdateMessageStatus atualiza o status de uma mensagem após a tentativa de envio
func (d *Database) UpdateMessageStatus(ctx context.Context, msg Message, status int) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	query := `
		UPDATE msg 
		SET status = ?
		WHERE para = ? 
		AND gerada_em = ? 
		AND destino = ? 
		AND mensagem = ?
	`

	// Converte a data para o formato ISO
	geradaEmStr := msg.GeradaEm.Format("2006-01-02 15:04:05")

	result, err := d.db.ExecContext(ctx, query, status, msg.Para, geradaEmStr, msg.Destino, msg.Mensagem)
	if err != nil {
		return fmt.Errorf("erro ao atualizar status da mensagem: %v", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("erro ao verificar linhas afetadas: %v", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("nenhuma mensagem foi atualizada para %s (gerada em %s)", msg.Para, msg.GeradaEm)
	}

	return nil
}