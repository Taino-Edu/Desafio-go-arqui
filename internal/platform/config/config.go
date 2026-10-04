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
