package app

import "context"

type correlationKey struct{}

// WithCorrelationID guarda o identificador de rastreio da operação. Ele
// acompanha logs e vai para o envelope dos eventos publicados.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

// CorrelationID devolve o identificador de rastreio, ou "" se não houver.
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}
