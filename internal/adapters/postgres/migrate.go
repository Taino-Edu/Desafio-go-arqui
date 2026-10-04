// Package postgres contém o acesso ao PostgreSQL (pgx, SQL explícito).
package postgres

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // driver "pgx5://"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/Taino-Edu/Desafio-go-arqui/migrations"
)

// newMigrator cria um migrator a partir das migrations embutidas.
// databaseURL usa o formato postgres://usuario:senha@host:porta/banco.
func newMigrator(databaseURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("migrations source: %w", err)
	}
	u, err := url.Parse(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err)
	}
	u.Scheme = "pgx5" // o driver do golang-migrate para pgx v5 usa este esquema
	m, err := migrate.NewWithSourceInstance("iofs", src, u.String())
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return m, nil
}

func closeMigrator(m *migrate.Migrate, err error) error {
	srcErr, dbErr := m.Close()
	return errors.Join(err, srcErr, dbErr)
}

// MigrateUp aplica todas as migrations pendentes. Deve rodar com o papel
// dono do schema (wallet_owner), nunca com o papel da aplicação.
func MigrateUp(databaseURL string) (err error) {
	m, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer func() { err = closeMigrator(m, err) }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// MigrateDown reverte `steps` migrations (todas, se steps <= 0).
func MigrateDown(databaseURL string, steps int) (err error) {
	m, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer func() { err = closeMigrator(m, err) }()
	if steps > 0 {
		err = m.Steps(-steps)
	} else {
		err = m.Down()
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// MigrationVersion devolve a versão aplicada e se o banco ficou "sujo"
// (migration interrompida no meio).
func MigrationVersion(databaseURL string) (version uint, dirty bool, err error) {
	m, err := newMigrator(databaseURL)
	if err != nil {
		return 0, false, err
	}
	defer func() { err = closeMigrator(m, err) }()
	version, dirty, err = m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return version, dirty, err
}
