//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/fx"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/httpapi"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/sqsconsumer"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/config"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/idptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/sqstest"
)

// spansOf devolve os spans encerrados de um trace.
func spansOf(rec *tracetest.SpanRecorder, traceID string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.SpanContext().TraceID().String() == traceID {
			out = append(out, s)
		}
	}
	return out
}

func waitSpan(t *testing.T, rec *tracetest.SpanRecorder, traceID, prefix string, n int) []sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got []sdktrace.ReadOnlySpan
		for _, s := range spansOf(rec, traceID) {
			if strings.HasPrefix(s.Name(), prefix) {
				got = append(got, s)
			}
		}
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("spans %q no trace %s: %d, want %d", prefix, traceID, len(got), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Um trace atravessa a aplicação inteira: requisição HTTP (continuando o
// traceparent do cliente) -> comandos SQL -> outbox (trace_parent gravado na
// mesma transação) -> publicação no SQS, mais tarde, por um worker -> atributo
// traceparent na mensagem publicada. E na entrada: mensagem SQS com
// traceparent -> span do consumidor -> SQL.
func TestTracing_EndToEnd(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	db := pgtest.New(t)
	in, events := sqstest.New(t, 5*time.Second, 5), sqstest.New(t, 5*time.Second, 5)
	a, _ := apptest.StartOn(t, db, func(c *config.Config) {
		withSQS(in)(c)
		withOutbox(events)(c)
	}, fx.Decorate(func(trace.TracerProvider) trace.TracerProvider { return tp }))

	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "100.00")

	// --- HTTP -> SQL -> outbox -> SQS ---
	const clientTrace, clientSpan = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	r := a.Client.As(idptest.Token(t, "provider-a")).Do("POST", "/wagering/transactions", apptest.Op{
		Provider: "provider-a", ExternalID: "bet-1", PlayerID: player, WalletID: wallet,
		Round: "r", Game: "g", Kind: "BET", Amount: "10.00",
	}.Body(), "Idempotency-Key", "k-1", "traceparent", "00-"+clientTrace+"-"+clientSpan+"-01")
	apptest.Must(t, r, 201, "bet")

	httpSpan := waitSpan(t, rec, clientTrace, "POST ", 1)[0]
	if httpSpan.Name() != "POST /wagering/transactions" || httpSpan.Parent().SpanID().String() != clientSpan {
		t.Fatalf("span HTTP = %q parent %s", httpSpan.Name(), httpSpan.Parent().SpanID())
	}
	httpID := httpSpan.SpanContext().SpanID()
	// withCorrelation roda DENTRO de withTracing: o span leva o correlationId
	var corr string
	for _, kv := range httpSpan.Attributes() {
		if kv.Key == "correlation.id" {
			corr = kv.Value.AsString()
		}
	}
	if corr == "" || corr != r.Header.Get(httpapi.HeaderCorrelationID) {
		t.Errorf("correlation.id do span = %q, resposta %q", corr, r.Header.Get(httpapi.HeaderCorrelationID))
	}

	var dbSpans int
	for _, s := range spansOf(rec, clientTrace) {
		if strings.HasPrefix(s.Name(), "db ") {
			dbSpans++
			if s.Parent().SpanID() != httpID {
				t.Errorf("%s fora da requisição: parent %s", s.Name(), s.Parent().SpanID())
			}
		}
	}
	if dbSpans < 3 { // BEGIN/locks, INSERTs, UPDATE, COMMIT...
		t.Errorf("spans SQL = %d", dbSpans)
	}

	// a outbox guardou o contexto do span HTTP, junto com os eventos
	wantParent := "00-" + clientTrace + "-" + httpID.String() + "-01"
	if n := apptest.Count(t, db, `SELECT count(*) FROM outbox_events WHERE trace_parent = $1`, wantParent); n != 2 {
		t.Errorf("eventos com trace_parent da requisição = %d, want 2", n)
	}

	// o worker publicou continuando o trace; a mensagem leva o span "publish"
	pubs := waitSpan(t, rec, clientTrace, "publish ", 2)
	pubIDs := map[string]bool{}
	for _, s := range pubs {
		if s.Parent().SpanID() != httpID || s.SpanKind() != trace.SpanKindProducer {
			t.Errorf("%s: parent %s kind %v", s.Name(), s.Parent().SpanID(), s.SpanKind())
		}
		pubIDs[s.SpanContext().SpanID().String()] = true
	}
	var traced int
	total := apptest.Count(t, db, `SELECT count(*) FROM outbox_events`)
	for _, m := range events.ReadQueue(total, 10*time.Second) {
		tpAttr := m.Attributes["traceparent"]
		if tpAttr == "" {
			t.Errorf("mensagem sem traceparent: %s", m.Body)
			continue
		}
		parts := strings.Split(tpAttr, "-")
		if parts[1] == clientTrace {
			traced++
			if !pubIDs[parts[2]] {
				t.Errorf("traceparent %s não aponta para um span publish", tpAttr)
			}
		}
	}
	if traced != 2 {
		t.Errorf("mensagens no trace do cliente = %d, want 2", traced)
	}

	// o log da requisição leva o traceId (do log se chega ao trace)
	var logged bool
	for _, l := range a.Logs.Find("http request") {
		if l["traceId"] == clientTrace && l["path"] == "/wagering/transactions" {
			logged = true
		}
	}
	if !logged {
		t.Error("log da requisição sem traceId")
	}

	// --- SQS (com traceparent do produtor) -> consumidor -> SQL ---
	const producerTrace, producerSpan = "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331"
	op := apptest.Op{Provider: "provider-a", ExternalID: "bet-2", PlayerID: player, WalletID: wallet,
		Round: "r", Game: "g", Kind: "BET", Amount: "5.00"}
	in.SendWithAttributes(sqstest.Message("msg-traced", data(op, "")), wallet, sqstest.Unique("dedup"),
		map[string]string{"traceparent": "00-" + producerTrace + "-" + producerSpan + "-01"})
	a.Client.WaitStatus("provider-a", "bet-2", "PROCESSED", 10*time.Second)

	proc := waitSpan(t, rec, producerTrace, "process ", 1)[0]
	if proc.Name() != "process "+sqsconsumer.MessageType || proc.SpanKind() != trace.SpanKindConsumer ||
		proc.Parent().SpanID().String() != producerSpan || !proc.Parent().IsRemote() {
		t.Errorf("span do consumidor = %q kind %v parent %s", proc.Name(), proc.SpanKind(), proc.Parent().SpanID())
	}
	var consumerSQL int
	for _, s := range spansOf(rec, producerTrace) {
		if strings.HasPrefix(s.Name(), "db ") && s.Parent().SpanID() == proc.SpanContext().SpanID() {
			consumerSQL++
		}
	}
	if consumerSQL < 3 {
		t.Errorf("spans SQL do consumidor = %d", consumerSQL)
	}
	if n := apptest.Count(t, db, `SELECT count(*) FROM outbox_events WHERE trace_parent LIKE $1`, "00-"+producerTrace+"-%"); n != 2 {
		t.Errorf("eventos da mensagem no trace do produtor = %d, want 2", n)
	}
}
