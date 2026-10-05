// Comando migrate aplica ou reverte as migrations do banco.
//
//	go run ./cmd/migrate up
//	go run ./cmd/migrate to 5      # leva o schema até a versão 5 (implantação em etapas)
//	go run ./cmd/migrate down 1     # reverte a última migration
//	go run ./cmd/migrate down all   # reverte tudo (apaga os dados!)
//	go run ./cmd/migrate version
//
// Lê MIGRATE_DATABASE_URL (papel dono do schema, wallet_owner).
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/postgres"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dbURL := os.Getenv("MIGRATE_DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("MIGRATE_DATABASE_URL is required")
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: migrate up | to <version> | down <n|all> | version")
	}

	switch args[0] {
	case "up":
		if err := postgres.MigrateUp(dbURL); err != nil {
			return err
		}
	case "to":
		if len(args) < 2 {
			return fmt.Errorf("to requires a version")
		}
		v, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil || v < 1 {
			return fmt.Errorf("invalid version %q", args[1])
		}
		if err := postgres.MigrateTo(dbURL, uint(v)); err != nil {
			return err
		}
	case "down":
		if len(args) < 2 {
			return fmt.Errorf("down requires a step count or \"all\"")
		}
		steps := 0
		if args[1] != "all" {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				return fmt.Errorf("invalid step count %q", args[1])
			}
			steps = n
		}
		if err := postgres.MigrateDown(dbURL, steps); err != nil {
			return err
		}
	case "version":
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}

	v, dirty, err := postgres.MigrationVersion(dbURL)
	if err != nil {
		return err
	}
	fmt.Printf("schema version: %d (dirty: %t)\n", v, dirty)
	return nil
}
