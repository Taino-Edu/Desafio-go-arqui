// Package domainerr define erros de domínio compartilhados e classificáveis
// com errors.Is / errors.As.
package domainerr

import (
	"errors"
	"fmt"
)

var (
	// ErrValidation agrupa toda entrada estruturalmente inválida (campo vazio,
	// ID nulo, valor fora da regra). O adaptador HTTP a mapeia para 400.
	ErrValidation = errors.New("validation failed")

	// ErrUninitialized indica uso de uma entidade de valor zero, não construída.
	ErrUninitialized = errors.New("uninitialized domain value")
)

// FieldError descreve qual campo falhou e por quê.
// errors.Is(err, ErrValidation) é true para qualquer FieldError.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("validation failed: %s: %s", e.Field, e.Reason)
}

func (e *FieldError) Is(target error) bool { return target == ErrValidation }

// Field cria um FieldError.
func Field(field, reason string) error {
	return &FieldError{Field: field, Reason: reason}
}
