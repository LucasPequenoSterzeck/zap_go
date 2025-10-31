package main

// $env:GOOS="windows"; $env:GOARCH="amd64"; go build -o zap_win.exe

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite"
)

const (
	messageCheckInterval = 5 * time.Second  // Intervalo entre verificações do banco
	messageDelay = 5 * time.Second          // Delay entre mensagens (aumentado para 5s)
	maxMessagesPerBatch = 30                // Número máximo de mensagens por lote
	dbPath = "./banco/msg.db"
)

// Cache de grupos para evitar consultas repetidas
type GroupCache struct {
	groups     []*types.GroupInfo
	lastUpdate time.Time
	mu         sync.RWMutex
}

// EventHandler implementa os callbacks para eventos do WhatsApp
type EventHandler struct {
	client      *whatsmeow.Client
	db          *Database
	groupCache  *GroupCache
	cacheMutex  sync.Mutex
}

// HandleEvent lida com eventos do WhatsApp
func (handler *EventHandler) HandleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Connected:
		log.Printf("=== Evento Connected recebido! ===")
	case *events.LoggedOut:
		log.Printf("=== Evento LoggedOut recebido! Razão: %v ===", v.Reason)
	case *events.Message:
		log.Printf("=== Nova mensagem recebida de %s ===", v.Info.Sender)
	}
}

// processMessageQueue processa a fila de mensagens pendentes
func (handler *EventHandler) processMessageQueue(ctx context.Context) {
	log.Println("=== Função processMessageQueue iniciada ===")
	ticker := time.NewTicker(messageCheckInterval)
	defer ticker.Stop()
	
	ciclo := 0
	for {
		ciclo++
		select {
		case <-ctx.Done():
			log.Println("Contexto cancelado, encerrando processMessageQueue")
			return
		case <-ticker.C:
			log.Printf("Ciclo %d: Verificando mensagens pendentes...", ciclo)
			messages, err := handler.db.GetPendingMessages(ctx)
			if err != nil {
				log.Printf("Erro ao buscar mensagens pendentes: %v", err)
				continue
			}

			if len(messages) > 0 {
				log.Printf("Processando lote de %d mensagens...", len(messages))
			}

			for i, msg := range messages {
				log.Printf("Processando mensagem %d/%d - Destinatário: %s", i+1, len(messages), msg.Para)
				
				// Adiciona delay entre mensagens
				time.Sleep(messageDelay)

				err := handler.sendMessage(ctx, msg)
				if err != nil {
					log.Printf("❌ Erro ao enviar mensagem para %s: %v", msg.Para, err)
					// Atualiza status para erro (2)
					if updateErr := handler.db.UpdateMessageStatus(ctx, msg, 2); updateErr != nil {
						log.Printf("Erro ao atualizar status da mensagem: %v", updateErr)
					}
					continue
				}

				log.Printf("✅ Mensagem enviada com sucesso para %s", msg.Para)

				// Atualiza status para enviado com sucesso (1)
				if err := handler.db.UpdateMessageStatus(ctx, msg, 1); err != nil {
					log.Printf("Erro ao atualizar status da mensagem: %v", err)
				}
			}

			if len(messages) > 0 {
				log.Printf("Lote de mensagens processado. Aguardando próximo ciclo...")
			}
		}
	}
}

// sendMessage envia uma mensagem individual
func (handler *EventHandler) sendMessage(ctx context.Context, msg Message) error {
	var targetJID types.JID

	if msg.Destino == "Grupo" {
		// Primeiro tenta usar o cache
		handler.groupCache.mu.RLock()
		cacheAge := time.Since(handler.groupCache.lastUpdate)
		foundInCache := false
		
		// Se o cache tem menos de 5 minutos, usa ele
		if cacheAge < 5*time.Minute && len(handler.groupCache.groups) > 0 {
			for _, group := range handler.groupCache.groups {
				if group.Name == msg.Para {
					targetJID = group.JID
					foundInCache = true
					break
				}
			}
		}
		handler.groupCache.mu.RUnlock()
		
		if foundInCache {
			log.Printf("Grupo encontrado no cache (idade: %v)", cacheAge.Round(time.Second))
		} else {
			// Se não encontrou no cache, atualiza a lista de grupos
			log.Printf("Atualizando cache de grupos...")
			
			// Implementa retry com backoff exponencial
			maxRetries := 3 // Reduzido para 3 tentativas
			baseDelay := 5 * time.Second // Aumentado para 5 segundos
			var lastErr error
			var groups []*types.GroupInfo
			
			for attempt := 0; attempt < maxRetries; attempt++ {
				if attempt > 0 {
					delay := baseDelay * time.Duration(1<<uint(attempt))
					log.Printf("Aguardando %v antes de tentar atualizar grupos (tentativa %d/%d)...", delay, attempt+1, maxRetries)
					time.Sleep(delay)
				}
				
				var err error
				groups, err = handler.client.GetJoinedGroups(ctx)
				if err == nil {
					// Atualiza o cache
					handler.groupCache.mu.Lock()
					handler.groupCache.groups = groups
					handler.groupCache.lastUpdate = time.Now()
					handler.groupCache.mu.Unlock()
					
					// Procura o grupo na nova lista
					for _, group := range groups {
						if group.Name == msg.Para {
							targetJID = group.JID
							foundInCache = true
							break
						}
					}
					
					if foundInCache {
						log.Printf("Grupo encontrado após atualização do cache")
						break
					}
					
					lastErr = fmt.Errorf("grupo não encontrado: %s", msg.Para)
					break
				}
				
				if !strings.Contains(strings.ToLower(err.Error()), "rate-overlimit") {
					return fmt.Errorf("erro ao buscar grupos: %v", err)
				}
				
				lastErr = err
			}
			
			if !foundInCache {
				return fmt.Errorf("erro após %d tentativas: %v", maxRetries, lastErr)
			}
		}
	} else {
		// Formata o número para o padrão WhatsApp (adiciona código do país se necessário)
		number := msg.Para
		if !strings.HasPrefix(number, "55") {
			number = "55" + number
		}
		targetJID = types.NewJID(number, types.DefaultUserServer)
	}

	// Prepara a mensagem
	content := &waProto.Message{
		Conversation: proto.String(msg.Mensagem),
	}

	// Se houver anexo, adiciona à mensagem
	if msg.Anexo.Valid && msg.Anexo.String != "" {
		// Lê o arquivo
		data, err := os.ReadFile(msg.Anexo.String)
		if err != nil {
			return fmt.Errorf("erro ao ler arquivo: %v", err)
		}

		// Detecta o tipo do arquivo pela extensão
		ext := strings.ToLower(filepath.Ext(msg.Anexo.String))
		mimeType := ""
		mediaType := whatsmeow.MediaImage // Padrão para imagens
		
		switch ext {
		case ".jpg", ".jpeg":
			mimeType = "image/jpeg"
			mediaType = whatsmeow.MediaImage
			content = &waProto.Message{
				ImageMessage: &waProto.ImageMessage{
					Caption:       proto.String(msg.Mensagem),
					Mimetype:     proto.String(mimeType),
					FileLength:   proto.Uint64(uint64(len(data))),
					FileSHA256:   make([]byte, 32),
					FileEncSHA256: make([]byte, 32),
					MediaKey:     make([]byte, 32),
				},
			}
		case ".png":
			mimeType = "image/png"
			mediaType = whatsmeow.MediaImage
			content = &waProto.Message{
				ImageMessage: &waProto.ImageMessage{
					Caption:       proto.String(msg.Mensagem),
					Mimetype:     proto.String(mimeType),
					FileLength:   proto.Uint64(uint64(len(data))),
					FileSHA256:   make([]byte, 32),
					FileEncSHA256: make([]byte, 32),
					MediaKey:     make([]byte, 32),
				},
			}
		case ".pdf":
			mimeType = "application/pdf"
			mediaType = whatsmeow.MediaDocument
			content = &waProto.Message{
				DocumentMessage: &waProto.DocumentMessage{
					Caption:       proto.String(msg.Mensagem),
					FileName:     proto.String(filepath.Base(msg.Anexo.String)),
					Mimetype:     proto.String(mimeType),
					FileLength:   proto.Uint64(uint64(len(data))),
					FileSHA256:   make([]byte, 32),
					FileEncSHA256: make([]byte, 32),
					MediaKey:     make([]byte, 32),
				},
			}
		case ".mp4", ".mkv", ".avi":
			mimeType = "video/mp4"
			mediaType = whatsmeow.MediaVideo
			content = &waProto.Message{
				VideoMessage: &waProto.VideoMessage{
					Caption:       proto.String(msg.Mensagem),
					Mimetype:     proto.String(mimeType),
					FileLength:   proto.Uint64(uint64(len(data))),
					FileSHA256:   make([]byte, 32),
					FileEncSHA256: make([]byte, 32),
					MediaKey:     make([]byte, 32),
				},
			}
		case ".mp3", ".wav", ".ogg":
			mimeType = "audio/mpeg"
			mediaType = whatsmeow.MediaAudio
			content = &waProto.Message{
				AudioMessage: &waProto.AudioMessage{
					Mimetype:     proto.String(mimeType),
					FileLength:   proto.Uint64(uint64(len(data))),
					FileSHA256:   make([]byte, 32),
					FileEncSHA256: make([]byte, 32),
					MediaKey:     make([]byte, 32),
				},
			}
		default:
			// Para qualquer outro tipo de arquivo, envia como documento
			mimeType = "application/octet-stream"
			mediaType = whatsmeow.MediaDocument
			content = &waProto.Message{
				DocumentMessage: &waProto.DocumentMessage{
					Caption:       proto.String(msg.Mensagem),
					FileName:     proto.String(filepath.Base(msg.Anexo.String)),
					Mimetype:     proto.String(mimeType),
					FileLength:   proto.Uint64(uint64(len(data))),
					FileSHA256:   make([]byte, 32),
					FileEncSHA256: make([]byte, 32),
					MediaKey:     make([]byte, 32),
				},
			}
		}

		// Envia a mensagem com o anexo
		uploadResponse, err := handler.client.Upload(ctx, data, mediaType)
		if err != nil {
			return fmt.Errorf("erro ao fazer upload do arquivo: %v", err)
		}

		// Atualiza a mensagem com as informações do upload
		switch {
		case content.ImageMessage != nil:
			content.ImageMessage.URL = proto.String(uploadResponse.URL)
			content.ImageMessage.DirectPath = proto.String(uploadResponse.DirectPath)
			content.ImageMessage.MediaKey = uploadResponse.MediaKey
			content.ImageMessage.FileEncSHA256 = uploadResponse.FileEncSHA256
			content.ImageMessage.FileSHA256 = uploadResponse.FileSHA256
		case content.DocumentMessage != nil:
			content.DocumentMessage.URL = proto.String(uploadResponse.URL)
			content.DocumentMessage.DirectPath = proto.String(uploadResponse.DirectPath)
			content.DocumentMessage.MediaKey = uploadResponse.MediaKey
			content.DocumentMessage.FileEncSHA256 = uploadResponse.FileEncSHA256
			content.DocumentMessage.FileSHA256 = uploadResponse.FileSHA256
		case content.VideoMessage != nil:
			content.VideoMessage.URL = proto.String(uploadResponse.URL)
			content.VideoMessage.DirectPath = proto.String(uploadResponse.DirectPath)
			content.VideoMessage.MediaKey = uploadResponse.MediaKey
			content.VideoMessage.FileEncSHA256 = uploadResponse.FileEncSHA256
			content.VideoMessage.FileSHA256 = uploadResponse.FileSHA256
		case content.AudioMessage != nil:
			content.AudioMessage.URL = proto.String(uploadResponse.URL)
			content.AudioMessage.DirectPath = proto.String(uploadResponse.DirectPath)
			content.AudioMessage.MediaKey = uploadResponse.MediaKey
			content.AudioMessage.FileEncSHA256 = uploadResponse.FileEncSHA256
			content.AudioMessage.FileSHA256 = uploadResponse.FileSHA256
		}

		// Envia a mensagem com o anexo
		_, err = handler.client.SendMessage(ctx, targetJID, content)
		if err != nil {
			return fmt.Errorf("erro ao enviar mensagem com anexo: %v", err)
		}
	} else {
		// Envia mensagem de texto normal com retry
		var sendErr error
		maxSendRetries := 3
		baseSendDelay := 2 * time.Second
		
		for attempt := 0; attempt < maxSendRetries; attempt++ {
			if attempt > 0 {
				delay := baseSendDelay * time.Duration(1<<uint(attempt))
				log.Printf("Aguardando %v antes de tentar reenviar (tentativa %d/%d)...", delay, attempt+1, maxSendRetries)
				time.Sleep(delay)
			}
			
			_, err := handler.client.SendMessage(ctx, targetJID, content)
			if err == nil {
				return nil // Mensagem enviada com sucesso
			}
			
			if strings.Contains(strings.ToLower(err.Error()), "rate-overlimit") {
				sendErr = err
				continue // Tenta novamente após o delay
			}
			
			return fmt.Errorf("erro ao enviar mensagem: %v", err)
		}
		
		return fmt.Errorf("erro após %d tentativas de envio: %v", maxSendRetries, sendErr)
	}

	return nil
}

func main() {
	// Configura o formato do log para incluir hora
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	
	log.Println("=== Iniciando programa ===")
	
	
	dbLog := waLog.Stdout("Database", "DEBUG", true)
	clientLog := waLog.Stdout("Client", "DEBUG", true)

	// Criar diretório para o banco de dados
	dbDir := "session"
	if err := os.MkdirAll(dbDir, 0700); err != nil {
		log.Fatalf("Erro ao criar diretório: %v", err)
	}

	// Criar diretório para o banco de mensagens se não existir
	msgDbDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(msgDbDir, 0700); err != nil {
		log.Fatalf("Erro ao criar diretório do banco de mensagens: %v", err)
	}

	// Conectar ao banco de dados de mensagens
	db, err := NewDatabase(dbPath)
	if err != nil {
		log.Fatalf("Erro ao conectar ao banco de mensagens: %v", err)
	}
	defer db.Close()
	
	// Criar contexto com cancelamento principal
	ctx, cancel := context.WithCancel(context.Background())
	// Verifica se há mensagens no banco logo no início
	ctxCheck, cancelCheck := context.WithTimeout(context.Background(), 5*time.Second)
	msgs, err := db.GetPendingMessages(ctxCheck)
	cancelCheck()
	if err != nil {
		log.Printf("Erro ao verificar mensagens iniciais: %v", err)
	} else {
		log.Printf("Status inicial: Encontradas %d mensagens pendentes no banco", len(msgs))
	}
	defer cancel()

	// Conectar ao banco de dados SQLite do WhatsApp
	container, err := sqlstore.New(ctx, "sqlite", "file:"+filepath.Join(dbDir, "store.db")+"?_pragma=foreign_keys(1)", dbLog)
	if err != nil {
		log.Fatalf("Erro ao conectar ao banco de dados do WhatsApp: %v", err)
	}

	// Get device store
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Fatalf("Erro ao obter device store: %v", err)
	}

	// Criar cliente
	client := whatsmeow.NewClient(deviceStore, clientLog)
	eventHandler := &EventHandler{
		client: client,
		db:     db,
		groupCache: &GroupCache{
			groups: make([]*types.GroupInfo, 0),
		},
	}
	client.AddEventHandler(eventHandler.HandleEvent)

	// Canal para sinalizar que a conexão foi estabelecida
	connected := make(chan bool, 1) // Adicionando buffer de 1 para evitar deadlock

	// Criar WaitGroup para gerenciar goroutines
	var wg sync.WaitGroup

	// Registra handler para eventos de conexão
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Connected:
			log.Println("Evento Connected recebido!")
			connected <- true
		case *events.LoggedOut:
			log.Printf("Evento LoggedOut recebido: %v", v)
		}
	})

	if client.Store.ID == nil {
		// No ID stored, new login required
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			log.Fatalf("Erro ao conectar: %v", err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				qr, err := qrcode.New(evt.Code, qrcode.Medium)
				if err != nil {
					log.Printf("Erro ao gerar QR code: %v", err)
					fmt.Printf("QR Code (texto): %s\n", evt.Code)
				} else {
					fmt.Println("\nPor favor, escaneie o QR Code abaixo com seu WhatsApp:")
					fmt.Println()
					// Gerar QR code em ASCII art
					art := qr.ToSmallString(false)
					fmt.Print("\x1b[34m") // Cor azul
					fmt.Print(art)
					fmt.Print("\x1b[0m") // Resetar cor
					fmt.Println("\nAguardando conexão...")
				}
			} else if evt.Event == "success" {
				fmt.Println("Login realizado com sucesso!")
				connected <- true
			} else {
				fmt.Printf("Login status: %s\n", evt.Event)
			}
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			log.Fatalf("Erro ao conectar: %v", err)
		}
		// Se já estava logado, sinaliza conexão estabelecida
		connected <- true
	}

	// Aguarda a conexão ser estabelecida
	log.Println("Aguardando conexão ser estabelecida...")
	select {
	case <-connected:
		log.Println("Conexão estabelecida! Iniciando processamento de mensagens...")
	case <-time.After(60 * time.Second):
		log.Fatal("Timeout aguardando conexão")
	}
	
	// Pequena pausa para garantir que a conexão está totalmente estabelecida
	time.Sleep(2 * time.Second)
	
	// Verifica se há mensagens pendentes
	messages, err := eventHandler.db.GetPendingMessages(ctx)
	if err != nil {
		log.Printf("Erro ao verificar mensagens pendentes: %v", err)
	} else {
		log.Printf("Encontradas %d mensagens pendentes para processamento", len(messages))
	}
	
	// Inicia o processamento de mensagens em uma goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Println("Iniciando goroutine de processamento de mensagens...")
		eventHandler.processMessageQueue(ctx)
	}()
	
	log.Println("Processamento de mensagens iniciado com sucesso")

	// Listen to Ctrl+C
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	// Inicia o desligamento gracioso
	fmt.Println("\nDesligando graciosamente... (pressione Ctrl+C novamente para forçar)")
	
	// Cria um canal para timeout
	done := make(chan bool)
	go func() {
		cancel() // Cancela o contexto para parar o processamento de mensagens
		wg.Wait() // Espera as goroutines terminarem
		client.Disconnect()
		done <- true
	}()

	// Espera o desligamento gracioso ou força após 10 segundos
	select {
	case <-done:
		fmt.Println("Desligamento concluído com sucesso")
	case <-time.After(10 * time.Second):
		fmt.Println("Timeout - Forçando desligamento")
	case <-c:
		fmt.Println("Forçando desligamento")
	}
}