package config_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/config"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Addr != ":8080" || cfg.Database.MaxConns != 20 || cfg.LogLevel != slog.LevelInfo {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.HTTP.RequestTimeout >= cfg.ShutdownTimeout {
		t.Error("o padrão deve permitir concluir requisições no shutdown")
	}
}

func TestLoad_Overrides(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"DATABASE_URL": "postgres://x", "HTTP_ADDR": ":9999", "DB_MAX_CONNS": "5",
		"LOG_LEVEL": "debug", "SHUTDOWN_TIMEOUT": "40s", "INSTANCE_ID": "i-1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Addr != ":9999" || cfg.Database.MaxConns != 5 || cfg.LogLevel != slog.LevelDebug ||
		cfg.ShutdownTimeout != 40*time.Second || cfg.InstanceID != "i-1" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoad_Invalid(t *testing.T) {
	tests := map[string]struct {
		env  map[string]string
		want string
	}{
		"sem banco":            {map[string]string{}, "DATABASE_URL is required"},
		"duração inválida":     {map[string]string{"DATABASE_URL": "x", "DB_LOCK_TIMEOUT": "3"}, "DB_LOCK_TIMEOUT: invalid duration"},
		"inteiro inválido":     {map[string]string{"DATABASE_URL": "x", "DB_MAX_CONNS": "dez"}, "DB_MAX_CONNS: invalid integer"},
		"nível inválido":       {map[string]string{"DATABASE_URL": "x", "LOG_LEVEL": "loud"}, "LOG_LEVEL: invalid level"},
		"conexões zero":        {map[string]string{"DATABASE_URL": "x", "DB_MAX_CONNS": "0"}, "DB_MAX_CONNS must be >= 1"},
		"timeout negativo":     {map[string]string{"DATABASE_URL": "x", "HTTP_IDLE_TIMEOUT": "-1s"}, "HTTP_IDLE_TIMEOUT must be positive"},
		"requisição > desliga": {map[string]string{"DATABASE_URL": "x", "HTTP_REQUEST_TIMEOUT": "30s", "SHUTDOWN_TIMEOUT": "10s"}, "shorter than SHUTDOWN_TIMEOUT"},
		"lock > requisição":    {map[string]string{"DATABASE_URL": "x", "DB_LOCK_TIMEOUT": "20s"}, "DB_LOCK_TIMEOUT must be shorter"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want contendo %q", err, tt.want)
			}
		})
	}
}
