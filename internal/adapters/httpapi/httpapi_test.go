package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

func TestClassifyError(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{domainerr.Field("playerId", "required"), 400, CodeInvalidRequest},
		{money.ErrInvalidScale, 400, CodeInvalidRequest},
		{money.ErrInvalidCurrency, 400, CodeInvalidRequest},
		{app.ErrInvalidCursor, 400, CodeInvalidRequest},
		{fmt.Errorf("%w: x", errBadRequest), 400, CodeInvalidRequest},
		{app.ErrWalletNotFound, 404, CodeNotFound},
		{app.ErrWalletAlreadyExists, 409, CodeWalletAlreadyExists},
		{app.ErrTransactionNotFound, 404, CodeNotFound},
		{app.ErrUnauthenticated, 401, CodeUnauthenticated},
		{app.ErrForbidden, 403, CodeForbidden},
		{app.ErrIdempotencyKeyReused, 409, CodeIdempotencyKeyReused},
		{app.ErrDuplicateTransaction, 409, CodeDuplicateTransaction},
		{fmt.Errorf("%w: deadlock", app.ErrTransient), 503, CodeTemporarilyUnavailable},
		{context.DeadlineExceeded, 503, CodeTemporarilyUnavailable},
		{errors.New("boom"), 500, CodeInternal},
	}
	for _, tt := range tests {
		status, body := classifyError(tt.err)
		if status != tt.status || body.Error.Code != tt.code {
			t.Errorf("%v -> %d %s, want %d %s", tt.err, status, body.Error.Code, tt.status, tt.code)
		}
	}
	if _, body := classifyError(errors.New("pq: senha do banco xyz")); strings.Contains(body.Error.Message, "senha") {
		t.Error("erro 500 não pode expor detalhes internos")
	}
}

func TestDecodeJSON(t *testing.T) {
	type dst struct {
		Amount string `json:"amount"`
	}
	tests := map[string]struct{ body, want string }{
		"vazio":         {"", "empty body"},
		"malformado":    {`{"amount":`, "malformed JSON"},
		"tipo errado":   {`{"amount":10.5}`, `field "amount" must be a JSON string`},
		"campo extra":   {`{"amount":"1.00","x":1}`, `unknown field "x"`},
		"dois objetos":  {`{"amount":"1.00"}{"amount":"2.00"}`, "unexpected data"},
		"array":         {`[1]`, "body must be a JSON object"},
		"grande demais": {`{"amount":"` + strings.Repeat("9", maxBodyBytes) + `"}`, "body too large"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			var d dst
			err := decodeJSON(httptest.NewRecorder(), r, &d)
			if !errors.Is(err, errBadRequest) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want contendo %q", err, tt.want)
			}
			if strings.Contains(fmt.Sprint(err), "Go struct") {
				t.Error("mensagem expõe tipo interno")
			}
		})
	}
}

type fakeChecker struct{ err error }

func (fakeChecker) Name() string                  { return "db" }
func (f fakeChecker) Check(context.Context) error { return f.err }

func TestHealth(t *testing.T) {
	get := func(h http.HandlerFunc) (int, string) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		b, _ := io.ReadAll(rec.Body)
		return rec.Code, string(b)
	}

	ok := NewHealth(time.Second, fakeChecker{})
	if code, _ := get(ok.live); code != 200 {
		t.Errorf("live = %d", code)
	}
	if code, body := get(ok.ready); code != 200 || !strings.Contains(body, `"db":"ok"`) {
		t.Errorf("ready = %d %s", code, body)
	}

	down := NewHealth(time.Second, fakeChecker{err: errors.New("down")})
	if code, body := get(down.ready); code != 503 || !strings.Contains(body, `"db":"unavailable"`) {
		t.Errorf("ready com banco fora = %d %s", code, body)
	}
	if code, _ := get(down.live); code != 200 {
		t.Error("liveness não depende do banco")
	}

	ok.SetDraining()
	if code, body := get(ok.ready); code != 503 || !strings.Contains(body, "draining") {
		t.Errorf("ready durante shutdown = %d %s", code, body)
	}

	// regressão (achada no teste de caos do SQS): uma dependência lenta não
	// pode consumir o prazo das outras e derrubar a verificação do banco
	mixed := NewHealth(200*time.Millisecond, slowChecker{}, quickChecker{})
	if code, body := get(mixed.ready); code != 503 || !strings.Contains(body, `"db":"ok"`) ||
		!strings.Contains(body, `"slow":"unavailable"`) {
		t.Errorf("ready com uma dependência lenta = %d %s", code, body)
	}
}

// slowChecker só termina quando o prazo acaba (como um SQS retentando).
type slowChecker struct{}

func (slowChecker) Name() string { return "slow" }
func (slowChecker) Check(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// quickChecker responde na hora, mas respeita o prazo (como o ping do pgx).
type quickChecker struct{}

func (quickChecker) Name() string                    { return "db" }
func (quickChecker) Check(ctx context.Context) error { return ctx.Err() }

func TestCorrelationMiddleware(t *testing.T) {
	var seen string
	h := withCorrelation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = app.CorrelationID(r.Context())
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderCorrelationID, "abc-123")
	h.ServeHTTP(rec, req)
	if seen != "abc-123" || rec.Header().Get(HeaderCorrelationID) != "abc-123" {
		t.Errorf("propagado = %q / %q", seen, rec.Header().Get(HeaderCorrelationID))
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderCorrelationID, "evil\nlog-injection")
	h.ServeHTTP(rec, req)
	if strings.Contains(seen, "\n") || seen == "" {
		t.Errorf("id inválido deveria ser substituído: %q", seen)
	}
}

func TestRecoverMiddleware(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := withRecover(log, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("bug") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), CodeInternal) {
		t.Errorf("panic -> %d %s", rec.Code, rec.Body.String())
	}
}
