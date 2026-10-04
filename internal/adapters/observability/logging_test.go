package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

func TestContextHandler_AddsCorrelationID(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil))).With("service", "wallet")
	ctx := app.WithCorrelationID(context.Background(), "corr-42")

	log.InfoContext(ctx, "com contexto", "walletId", "w-1")
	log.With("component", "worker").WarnContext(ctx, "logger derivado")
	log.Info("sem contexto")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("linhas = %d:\n%s", len(lines), buf.String())
	}
	var recs []map[string]any
	for _, l := range lines {
		var r map[string]any
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	if recs[0]["correlationId"] != "corr-42" || recs[0]["walletId"] != "w-1" || recs[0]["service"] != "wallet" {
		t.Errorf("log com contexto = %v", recs[0])
	}
	if recs[1]["correlationId"] != "corr-42" || recs[1]["component"] != "worker" {
		t.Errorf("logger derivado (With) perdeu o embrulho: %v", recs[1])
	}
	if _, ok := recs[2]["correlationId"]; ok {
		t.Errorf("sem contexto não há correlationId: %v", recs[2])
	}
	// o id aparece uma vez só (nenhuma chave duplicada no JSON)
	if strings.Count(lines[0], `"correlationId"`) != 1 {
		t.Errorf("correlationId duplicado: %s", lines[0])
	}
}
