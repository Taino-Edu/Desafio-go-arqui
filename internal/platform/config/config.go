// Package config carrega e valida a configuração a partir de variáveis de
// ambiente. Uma configuração inválida impede a aplicação de iniciar.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config é a configuração completa do serviço.
type Config struct {
	InstanceID string
	LogLevel   slog.Level

	HTTP       HTTP
	Database   Database
	References References
	Auth       Auth
	SQS        SQS
	Outbox     Outbox

	StartTimeout    time.Duration // prazo para todas as dependências subirem
	ShutdownTimeout time.Duration // prazo para concluir o trabalho em andamento
}

type HTTP struct {
	Addr           string
	RequestTimeout time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	IdleTimeout    time.Duration
}

// Outbox configura o publicador da outbox. Usa a conexão SQS (região,
// endpoint, credenciais) da seção SQS.
type Outbox struct {
	Enabled        bool
	Queue          string        // fila de eventos (nome ou URL)
	BatchSize      int           // eventos reivindicados por rodada
	PollInterval   time.Duration // espera quando não há eventos
	Lease          time.Duration // reserva de um evento reivindicado
	RetryBaseDelay time.Duration // backoff de falha de publicação
	RetryMaxDelay  time.Duration
}

// SQS configura o acesso ao SQS e o consumidor da fila de entrada
// (Enabled liga o consumidor).
type SQS struct {
	Enabled          bool
	Region           string
	Endpoint         string // vazio na AWS; http://localhost:4566 no LocalStack
	AccessKeyID      string // opcional; vazio = cadeia padrão da AWS
	SecretAccessKey  string
	InputQueue       string // nome ou URL
	DLQ              string // nome ou URL
	ConsumerName     string // escopo da inbox
	Pollers          int
	MaxMessages      int
	WaitTime         time.Duration // long polling (0..20s)
	RetryBaseDelay   time.Duration // backoff da visibilidade em falha transitória
	RetryMaxDelay    time.Duration
	AllowedProviders []string // vazio = qualquer provedor
}

// Auth descreve o IdP OAuth 2.0/OIDC. Não há modo "sem autenticação".
type Auth struct {
	Issuer   string // iss esperado nos tokens
	Audience string // aud exigida
	JWKSURL  string // opcional: onde buscar as chaves (padrão: derivado do emissor)
}

// References configura o worker de referências pendentes.
type References struct {
	WorkerEnabled bool
	PollInterval  time.Duration // espera quando não há pendência vencida
	BaseDelay     time.Duration // backoff: min(BaseDelay * 2^n, MaxDelay)
	MaxDelay      time.Duration
	MaxAttempts   int // depois disso: REJECTED com REFERENCE_NOT_FOUND
}

type Database struct {
	URL              string
	MaxConns         int32
	StatementTimeout time.Duration
	LockTimeout      time.Duration
}

// FromEnv lê as variáveis de ambiente, aplica os padrões e valida.
func FromEnv() (Config, error) {
	return Load(os.Getenv)
}

// Load é FromEnv com a fonte de variáveis injetável (para testes).
func Load(getenv func(string) string) (Config, error) {
	r := reader{get: getenv}
	host, _ := os.Hostname()
	cfg := Config{
		InstanceID: r.str("INSTANCE_ID", host),
		LogLevel:   r.level("LOG_LEVEL", slog.LevelInfo),
		HTTP: HTTP{
			Addr:           r.str("HTTP_ADDR", ":8080"),
			RequestTimeout: r.dur("HTTP_REQUEST_TIMEOUT", 10*time.Second),
			ReadTimeout:    r.dur("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:   r.dur("HTTP_WRITE_TIMEOUT", 20*time.Second),
			IdleTimeout:    r.dur("HTTP_IDLE_TIMEOUT", 60*time.Second),
		},
		Database: Database{
			URL:              r.str("DATABASE_URL", ""),
			MaxConns:         int32(r.int("DB_MAX_CONNS", 20)),
			StatementTimeout: r.dur("DB_STATEMENT_TIMEOUT", 5*time.Second),
			LockTimeout:      r.dur("DB_LOCK_TIMEOUT", 3*time.Second),
		},
		References: References{
			WorkerEnabled: r.bool("REFERENCE_WORKER_ENABLED", true),
			PollInterval:  r.dur("REFERENCE_WORKER_INTERVAL", time.Second),
			BaseDelay:     r.dur("REFERENCE_RETRY_BASE_DELAY", time.Second),
			MaxDelay:      r.dur("REFERENCE_RETRY_MAX_DELAY", 5*time.Minute),
			MaxAttempts:   r.int("REFERENCE_RETRY_MAX_ATTEMPTS", 12),
		},
		SQS: SQS{
			Enabled:          r.bool("SQS_ENABLED", true),
			Region:           r.str("AWS_REGION", "us-east-1"),
			Endpoint:         r.str("SQS_ENDPOINT", ""),
			AccessKeyID:      r.str("SQS_ACCESS_KEY_ID", ""),
			SecretAccessKey:  r.str("SQS_SECRET_ACCESS_KEY", ""),
			InputQueue:       r.str("SQS_INPUT_QUEUE", "wager-transactions.fifo"),
			DLQ:              r.str("SQS_INPUT_DLQ", "wager-transactions-dlq.fifo"),
			ConsumerName:     r.str("SQS_CONSUMER_NAME", "wager-transactions-consumer"),
			Pollers:          r.int("SQS_POLLERS", 2),
			MaxMessages:      r.int("SQS_MAX_MESSAGES", 10),
			WaitTime:         r.dur("SQS_WAIT_TIME", 10*time.Second),
			RetryBaseDelay:   r.dur("SQS_RETRY_BASE_DELAY", time.Second),
			RetryMaxDelay:    r.dur("SQS_RETRY_MAX_DELAY", time.Minute),
			AllowedProviders: r.list("SQS_ALLOWED_PROVIDERS"),
		},
		Outbox: Outbox{
			Enabled:        r.bool("OUTBOX_PUBLISHER_ENABLED", true),
			Queue:          r.str("OUTBOX_QUEUE", "wallet-events.fifo"),
			BatchSize:      r.int("OUTBOX_BATCH_SIZE", 50),
			PollInterval:   r.dur("OUTBOX_POLL_INTERVAL", 500*time.Millisecond),
			Lease:          r.dur("OUTBOX_LEASE", 30*time.Second),
			RetryBaseDelay: r.dur("OUTBOX_RETRY_BASE_DELAY", time.Second),
			RetryMaxDelay:  r.dur("OUTBOX_RETRY_MAX_DELAY", 5*time.Minute),
		},
		Auth: Auth{
			Issuer:   r.str("OIDC_ISSUER", ""),
			Audience: r.str("OIDC_AUDIENCE", "wallet-api"),
			JWKSURL:  r.str("OIDC_JWKS_URL", ""),
		},
		StartTimeout:    r.dur("START_TIMEOUT", 30*time.Second),
		ShutdownTimeout: r.dur("SHUTDOWN_TIMEOUT", 25*time.Second),
	}
	if len(r.errs) > 0 {
		return Config{}, errors.Join(r.errs...)
	}
	return cfg, cfg.Validate()
}

// Validate confere coerência entre os valores.
func (c Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.Auth.Issuer == "" {
		errs = append(errs, errors.New("OIDC_ISSUER is required (authentication cannot be disabled)"))
	}
	if c.Database.MaxConns < 1 {
		errs = append(errs, errors.New("DB_MAX_CONNS must be >= 1"))
	}
	if c.HTTP.Addr == "" {
		errs = append(errs, errors.New("HTTP_ADDR is required"))
	}
	for name, d := range map[string]time.Duration{
		"HTTP_REQUEST_TIMEOUT": c.HTTP.RequestTimeout, "HTTP_READ_TIMEOUT": c.HTTP.ReadTimeout,
		"HTTP_WRITE_TIMEOUT": c.HTTP.WriteTimeout, "HTTP_IDLE_TIMEOUT": c.HTTP.IdleTimeout,
		"DB_STATEMENT_TIMEOUT": c.Database.StatementTimeout, "DB_LOCK_TIMEOUT": c.Database.LockTimeout,
		"START_TIMEOUT": c.StartTimeout, "SHUTDOWN_TIMEOUT": c.ShutdownTimeout,
		"REFERENCE_WORKER_INTERVAL":  c.References.PollInterval,
		"REFERENCE_RETRY_BASE_DELAY": c.References.BaseDelay,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}
	if c.HTTP.RequestTimeout >= c.ShutdownTimeout {
		errs = append(errs, errors.New("HTTP_REQUEST_TIMEOUT must be shorter than SHUTDOWN_TIMEOUT, "+
			"or in-flight requests cannot finish during shutdown"))
	}
	if c.References.MaxDelay < c.References.BaseDelay {
		errs = append(errs, errors.New("REFERENCE_RETRY_MAX_DELAY must be >= REFERENCE_RETRY_BASE_DELAY"))
	}
	if c.References.MaxAttempts < 1 {
		errs = append(errs, errors.New("REFERENCE_RETRY_MAX_ATTEMPTS must be >= 1"))
	}
	if c.SQS.Enabled {
		if c.SQS.InputQueue == "" || c.SQS.DLQ == "" || c.SQS.ConsumerName == "" {
			errs = append(errs, errors.New("SQS_INPUT_QUEUE, SQS_INPUT_DLQ and SQS_CONSUMER_NAME are required when SQS_ENABLED"))
		}
		if c.SQS.Pollers < 1 || c.SQS.MaxMessages < 1 || c.SQS.MaxMessages > 10 {
			errs = append(errs, errors.New("SQS_POLLERS must be >= 1 and SQS_MAX_MESSAGES between 1 and 10"))
		}
		if c.SQS.WaitTime < 0 || c.SQS.WaitTime > 20*time.Second {
			errs = append(errs, errors.New("SQS_WAIT_TIME must be between 0s and 20s"))
		}
		if c.SQS.RetryBaseDelay < time.Second || c.SQS.RetryMaxDelay < c.SQS.RetryBaseDelay || c.SQS.RetryMaxDelay > 12*time.Hour {
			errs = append(errs, errors.New("SQS_RETRY_BASE_DELAY must be >= 1s and <= SQS_RETRY_MAX_DELAY <= 12h"))
		}
	}
	if c.Outbox.Enabled {
		if c.Outbox.Queue == "" || c.Outbox.BatchSize < 1 || c.Outbox.BatchSize > 1000 {
			errs = append(errs, errors.New("OUTBOX_QUEUE is required and OUTBOX_BATCH_SIZE must be between 1 and 1000"))
		}
		if c.Outbox.PollInterval <= 0 || c.Outbox.Lease < time.Second {
			errs = append(errs, errors.New("OUTBOX_POLL_INTERVAL must be positive and OUTBOX_LEASE >= 1s"))
		}
		if c.Outbox.RetryBaseDelay <= 0 || c.Outbox.RetryMaxDelay < c.Outbox.RetryBaseDelay {
			errs = append(errs, errors.New("OUTBOX_RETRY_BASE_DELAY must be positive and <= OUTBOX_RETRY_MAX_DELAY"))
		}
	}
	if c.Database.LockTimeout >= c.HTTP.RequestTimeout {
		errs = append(errs, errors.New("DB_LOCK_TIMEOUT must be shorter than HTTP_REQUEST_TIMEOUT"))
	}
	return errors.Join(errs...)
}

type reader struct {
	get  func(string) string
	errs []error
}

func (r *reader) str(key, def string) string {
	if v := strings.TrimSpace(r.get(key)); v != "" {
		return v
	}
	return def
}

func (r *reader) dur(key string, def time.Duration) time.Duration {
	v := r.get(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: invalid duration %q", key, v))
	}
	return d
}

func (r *reader) int(key string, def int) int {
	v := r.get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: invalid integer %q", key, v))
	}
	return n
}

func (r *reader) list(key string) []string {
	var out []string
	for _, v := range strings.Split(r.get(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (r *reader) bool(key string, def bool) bool {
	v := r.get(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: invalid boolean %q", key, v))
	}
	return b
}

func (r *reader) level(key string, def slog.Level) slog.Level {
	v := r.get(key)
	if v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: invalid level %q", key, v))
	}
	return l
}
