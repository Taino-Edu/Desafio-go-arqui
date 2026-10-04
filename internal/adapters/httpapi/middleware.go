package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// HeaderCorrelationID propaga o identificador de rastreio entre serviços.
const HeaderCorrelationID = "X-Correlation-Id"

// só aceita IDs simples vindos de fora (evita injeção em logs)
var validCorrelation = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// withCorrelation usa o X-Correlation-Id recebido ou gera um novo, coloca no
// contexto e devolve no cabeçalho da resposta.
func withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderCorrelationID)
		if !validCorrelation.MatchString(id) {
			id = uuid.NewString()
		}
		w.Header().Set(HeaderCorrelationID, id)
		next.ServeHTTP(w, r.WithContext(app.WithCorrelationID(r.Context(), id)))
	})
}

// withTimeout limita o tempo de cada requisição; o contexto cancelado
// interrompe as consultas ao banco em andamento.
func withTimeout(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// withLogging registra uma linha por requisição, sem corpo (sem dados
// financeiros nem credenciais). O correlationId vem do contexto (o handler
// de log o acrescenta). Health checks e coletas de métricas chegam a cada
// poucos segundos: ficam em DEBUG para não afogar os logs.
func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		if isOperational(r.URL.Path) && rec.status < 500 {
			level = slog.LevelDebug
		}
		log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"durationMs", time.Since(start).Milliseconds(),
		)
	})
}

func isOperational(path string) bool {
	return strings.HasPrefix(path, "/health/") || path == "/metrics"
}

// withRecover transforma um panic inesperado em 500, sem derrubar o processo.
func withRecover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic in handler", "panic", v)
				writeJSON(w, http.StatusInternalServerError, errBody(CodeInternal, "internal error", ""))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
