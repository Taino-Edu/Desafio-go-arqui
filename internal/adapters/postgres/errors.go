package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// transientCodes são SQLSTATEs que indicam falha temporária: tentar de novo
// pode dar certo e nada foi confirmado.
var transientCodes = map[string]bool{
	"40001": true, // serialization_failure
	"40P01": true, // deadlock_detected
	"55P03": true, // lock_not_available (lock_timeout)
	"57014": true, // query_canceled (statement_timeout)
	"57P01": true, // admin_shutdown
	"57P02": true, // crash_shutdown
	"57P03": true, // cannot_connect_now
	"53300": true, // too_many_connections
	"53400": true, // configuration_limit_exceeded
}

// classify embrulha falhas temporárias com app.ErrTransient, preservando o
// erro original para logs (errors.Is/As continuam funcionando nos dois).
func classify(err error) error {
	if err == nil || errors.Is(err, app.ErrTransient) {
		return err
	}
	if isTransient(err) {
		return fmt.Errorf("%w: %w", app.ErrTransient, err)
	}
	return err
}

func isTransient(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// classe 08: problemas de conexão
		return transientCodes[pgErr.Code] || (len(pgErr.Code) == 5 && pgErr.Code[:2] == "08")
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return pgconn.SafeToRetry(err)
}

// uniqueViolation devolve o nome da constraint violada, se err for uma
// violação de unicidade.
func uniqueViolation(err error) (constraint string, ok bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}
