package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerName identifica a instrumentação desta aplicação nos spans.
const TracerName = "github.com/Taino-Edu/Desafio-go-arqui"

// Propagator é o formato de contexto de trace entre serviços: W3C Trace
// Context (cabeçalho e atributo traceparent) e Baggage.
var Propagator propagation.TextMapPropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{}, propagation.Baggage{})

// TracingConfig liga ou não o envio de spans.
type TracingConfig struct {
	Enabled    bool
	InstanceID string
}

// NewTracerProvider cria o provedor de spans. Desligado, devolve um provedor
// que não faz nada (custo praticamente zero). Ligado, exporta por OTLP/HTTP
// em lotes; endpoint, cabeçalhos e amostragem vêm das variáveis padrão do
// OpenTelemetry (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_TRACES_SAMPLER...).
// shutdown descarrega os spans pendentes: deve rodar no desligamento.
func NewTracerProvider(ctx context.Context, cfg TracingConfig) (tp trace.TracerProvider, shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("otlp exporter: %w", err)
	}
	res := resource.NewSchemaless(
		attribute.String("service.name", "wallet"),
		attribute.String("service.instance.id", cfg.InstanceID),
	)
	sdk := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	return sdk, sdk.Shutdown, nil
}
