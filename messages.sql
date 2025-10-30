CREATE TABLE IF NOT EXISTS msg (
    para TEXT NOT NULL,
    gerada_em TIMESTAMP NOT NULL,
    destino TEXT NOT NULL,
    mensagem TEXT NOT NULL,
    anexo TEXT,
    status INTEGER DEFAULT 0,
    meio TEXT DEFAULT 'WhatsApp',
    PRIMARY KEY (para, gerada_em, destino)
);