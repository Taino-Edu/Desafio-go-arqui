package postgres

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig configura o pool de conexões.
type PoolConfig struct {
	URL      string
	MaxConns int32
	// StatementTimeout limita cada comando SQL; LockTimeout limita a espera
	// por um lock de linha (ex.: SELECT ... FOR UPDATE de uma carteira
	// disputada). Estourar qualquer um vira app.ErrTransient.
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	ApplicationName  string
	// Tracer recebe um span por comando SQL (nil = sem tracing).
	Tracer pgx.QueryTracer
}

// NewPool cria o pool. Não abre conexão: as conexões são abertas sob
// demanda, e quem chama deve usar Ping para validar o banco na partida.
func NewPool(cfg PoolConfig) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse url: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	rp := pc.ConnConfig.RuntimeParams
	if cfg.StatementTimeout > 0 {
		rp["statement_timeout"] = strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
	}
	if cfg.LockTimeout > 0 {
		rp["lock_timeout"] = strconv.FormatInt(cfg.LockTimeout.Milliseconds(), 10)
	}
	if cfg.ApplicationName != "" {
		rp["application_name"] = cfg.ApplicationName
	}
	rp["timezone"] = "UTC"
	if cfg.Tracer != nil {
		pc.ConnConfig.Tracer = cfg.Tracer
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		return nil, fmt.Errorf("postgres: pool: %w", err)
	}
	return pool, nil
}

// Ping confirma que o banco responde.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: ping: %w", classify(err))
	}
	return nil
}

// HealthChecker verifica se o banco responde (readiness).
type HealthChecker struct{ Pool *pgxpool.Pool }

func (HealthChecker) Name() string { return "postgres" }

func (h HealthChecker) Check(ctx context.Context) error { return h.Pool.Ping(ctx) }
