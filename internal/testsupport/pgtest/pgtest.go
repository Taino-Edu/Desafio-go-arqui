// Package pgtest cria bancos PostgreSQL descartáveis para testes de
// integração, rodando contra o Postgres real do docker compose.
//
// Cada chamada de New cria um banco novo (test_<aleatório>), aplica as
// migrations com o papel dono do schema e devolve conexões como dono e como
// aplicação. O banco é apagado no fim do teste.
//
// Variável: TEST_PG_ADMIN_URL (superusuário). Padrão:
// postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/postgres"
)

const defaultAdminURL = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"

// Credenciais de desenvolvimento criadas por deploy/postgres/01-roles.sql.
var (
	ownerUser = url.UserPassword("wallet_owner", "wallet_owner")
	appUser   = url.UserPassword("wallet_app", "wallet_app")
)

// DB é um banco descartável já migrado.
type DB struct {
	Name     string
	OwnerURL string        // papel dono do schema (migrations)
	AppURL   string        // papel da aplicação
	Owner    *pgxpool.Pool // conexão como dono
	App      *pgxpool.Pool // conexão como aplicação
}

// New cria e migra um banco novo. Falha o teste se o Postgres não estiver
// acessível: rode `docker compose up -d postgres migrate` antes.
func New(t testing.TB) *DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminURL := os.Getenv("TEST_PG_ADMIN_URL")
	if adminURL == "" {
		adminURL = defaultAdminURL
	}
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("pgtest: conectar como admin (%s): %v\n"+
			"Suba o banco com: docker compose up -d postgres", redact(adminURL), err)
	}
	defer admin.Close(ctx)

	name := "test_" + randomSuffix()
	// identificador gerado aqui, só [a-z0-9_]: seguro para concatenar
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" OWNER wallet_owner"); err != nil {
		t.Fatalf("pgtest: criar banco: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			t.Logf("pgtest: limpar %s: %v", name, err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("pgtest: apagar %s: %v", name, err)
		}
	})

	// mesmo isolamento do banco "wallet" (ver 01-roles.sql)
	dbConn, err := pgx.Connect(ctx, withDatabase(adminURL, name, nil))
	if err != nil {
		t.Fatalf("pgtest: conectar no banco novo: %v", err)
	}
	for _, stmt := range []string{
		"REVOKE ALL ON DATABASE " + name + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + name + " TO wallet_app",
		"ALTER SCHEMA public OWNER TO wallet_owner",
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		"GRANT USAGE ON SCHEMA public TO wallet_app",
	} {
		if _, err := dbConn.Exec(ctx, stmt); err != nil {
			dbConn.Close(ctx)
			t.Fatalf("pgtest: %s: %v", stmt, err)
		}
	}
	dbConn.Close(ctx)

	db := &DB{
		Name:     name,
		OwnerURL: withDatabase(adminURL, name, ownerUser),
		AppURL:   withDatabase(adminURL, name, appUser),
	}
	if err := postgres.MigrateUp(db.OwnerURL); err != nil {
		t.Fatalf("pgtest: migrations: %v", err)
	}
	db.Owner = pool(ctx, t, db.OwnerURL)
	db.App = pool(ctx, t, db.AppURL)
	return db
}

func pool(ctx context.Context, t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgtest: pool: %v", err)
	}
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("pgtest: ping: %v", err)
	}
	t.Cleanup(p.Close) // roda antes do DROP DATABASE (ordem inversa)
	return p
}

func withDatabase(base, dbName string, user *url.Userinfo) string {
	u, _ := url.Parse(base)
	u.Path = "/" + dbName
	if user != nil {
		u.User = user
	}
	return u.String()
}

func redact(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "<invalid url>"
	}
	return u.Redacted()
}

func randomSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
