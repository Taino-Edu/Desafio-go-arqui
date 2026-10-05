// Comando loadtest gera carga contra uma ou mais instâncias da API e mede
// throughput, latência (p50/p95/p99), erros, conflitos de concorrência e o
// atraso da outbox. No fim, espera a outbox esvaziar e reconcilia todas as
// carteiras usadas: carga que deixa saldo errado não conta como sucesso.
//
//	docker compose up -d --build
//	go run ./cmd/loadtest -duration 60s -concurrency 32 -out docs/load/resultado.md
//
// Usa só a biblioteca padrão. Tokens reais do Keycloak (client_credentials).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	apis           []string
	idp            string
	duration       time.Duration
	warmup         time.Duration
	concurrency    int
	wallets        int
	hotFraction    float64
	replayFraction float64
	out            string
	label          string
	rate           float64
}

func main() {
	var c config
	var apis string
	flag.StringVar(&apis, "api", "http://localhost:8080", "URLs base das instâncias, separadas por vírgula (a carga é espalhada entre elas)")
	flag.StringVar(&c.idp, "idp", "http://localhost:8081/realms/wallet/protocol/openid-connect/token", "endpoint de token do Keycloak")
	flag.DurationVar(&c.duration, "duration", 60*time.Second, "duração da medição")
	flag.DurationVar(&c.warmup, "warmup", 5*time.Second, "aquecimento antes da medição (não entra nos números)")
	flag.IntVar(&c.concurrency, "concurrency", 32, "clientes simultâneos")
	flag.IntVar(&c.wallets, "wallets", 200, "carteiras usadas")
	flag.Float64Var(&c.hotFraction, "hot", 0.10, "fração das operações numa única carteira disputada (mede conflitos)")
	flag.Float64Var(&c.replayFraction, "replay", 0.05, "fração de reenvios de operações já feitas (mede replays)")
	flag.StringVar(&c.out, "out", "", "arquivo do relatório em Markdown (padrão: só stdout)")
	flag.StringVar(&c.label, "label", "", "rótulo do cenário no relatório")
	flag.Float64Var(&c.rate, "rate", 0, "requisições por segundo no total (0 = sem limite: mede a capacidade)")
	flag.Parse()
	for _, a := range strings.Split(apis, ",") {
		c.apis = append(c.apis, strings.TrimRight(strings.TrimSpace(a), "/"))
	}
	if err := run(c); err != nil {
		log.Fatal(err)
	}
}

// ------------------------------------------------------------------ HTTP

var client = &http.Client{
	Timeout:   30 * time.Second,
	Transport: &http.Transport{MaxIdleConnsPerHost: 512, MaxConnsPerHost: 0},
}

type tokens struct {
	mu   sync.RWMutex
	idp  string
	toks map[string]string
}

var secrets = map[string]string{
	"wallet-service": "wallet-service-secret",
	"provider-a":     "provider-a-secret",
	"provider-b":     "provider-b-secret",
}

func (t *tokens) refresh() error {
	fresh := map[string]string{}
	for id, secret := range secrets {
		resp, err := client.PostForm(t.idp, url.Values{
			"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret},
		})
		if err != nil {
			return fmt.Errorf("token %s: %w", id, err)
		}
		var body struct {
			AccessToken string `json:"access_token"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil || body.AccessToken == "" {
			return fmt.Errorf("token %s: status %d", id, resp.StatusCode)
		}
		fresh[id] = body.AccessToken
	}
	t.mu.Lock()
	t.toks = fresh
	t.mu.Unlock()
	return nil
}

func (t *tokens) get(id string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.toks[id]
}

func do(ctx context.Context, method, u, token string, body any, headers ...string) (int, map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, nil
}

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ------------------------------------------------------------------ carga

type wallet struct{ id, player string }

type op struct {
	provider, ext, key string
	body               map[string]any
}

// sample é uma requisição medida.
type sample struct {
	kind    string // BET, WIN, LOSS, REPLAY
	outcome string // created, replay, rejected, conflict409, unavailable503, other, transport
	latency time.Duration
}

type worker struct {
	samples []sample
	done    []op // operações confirmadas, para reenvio
}

func newOp(w wallet, kind, amount string) op {
	provider := "provider-a"
	if mrand.IntN(2) == 0 {
		provider = "provider-b"
	}
	ext := "lt-" + uuid()
	return op{provider: provider, ext: ext, key: provider + ":" + ext, body: map[string]any{
		"providerId": provider, "externalTransactionId": ext, "playerId": w.player, "walletId": w.id,
		"roundId": "round-" + strconv.Itoa(mrand.IntN(1000)), "gameId": "load-test", "kind": kind,
		"money": map[string]string{"amount": amount, "currency": "BRL"},
	}}
}

func classify(status int, body map[string]any, err error) string {
	switch {
	case err != nil:
		return "transport"
	case status == http.StatusCreated:
		return "created"
	case status == http.StatusOK && body["idempotentReplay"] == true:
		return "replay"
	case status == http.StatusUnprocessableEntity:
		return "rejected"
	case status == http.StatusConflict:
		return "conflict409"
	case status == http.StatusServiceUnavailable:
		return "unavailable503"
	default:
		return fmt.Sprintf("other%d", status)
	}
}

// ------------------------------------------------------------------ métricas do servidor

type scrape map[string]float64

func scrapeMetrics(api string) (scrape, error) {
	resp, err := client.Get(api + "/metrics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := scrape{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err == nil {
			out[line[:i]] = v
		}
	}
	return out, nil
}

// sum soma, em todas as instâncias, as séries cujo nome (com rótulos)
// começa por prefix.
func sum(scrapes []scrape, prefix string) float64 {
	var t float64
	for _, s := range scrapes {
		for k, v := range s {
			if strings.HasPrefix(k, prefix) {
				t += v
			}
		}
	}
	return t
}

func scrapeAll(apis []string) []scrape {
	var out []scrape
	for _, a := range apis {
		if s, err := scrapeMetrics(a); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// ------------------------------------------------------------------ execução

func run(c config) error {
	ctx := context.Background()
	tk := &tokens{idp: c.idp}
	if err := tk.refresh(); err != nil {
		return err
	}
	stopRefresh := make(chan struct{})
	go func() { // tokens duram 5 min
		t := time.NewTicker(3 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-stopRefresh:
				return
			case <-t.C:
				if err := tk.refresh(); err != nil {
					log.Printf("refresh de token: %v", err)
				}
			}
		}
	}()
	defer close(stopRefresh)

	api := func() string { return c.apis[mrand.IntN(len(c.apis))] }

	log.Printf("abrindo %d carteiras...", c.wallets)
	ws := make([]wallet, c.wallets)
	for i := range ws {
		ws[i].player = uuid()
		st, body, err := do(ctx, "POST", api()+"/wallets", tk.get("wallet-service"), map[string]any{
			"playerId": ws[i].player, "initialBalance": map[string]string{"amount": "1000000.00", "currency": "BRL"},
		})
		if err != nil || st != http.StatusCreated {
			return fmt.Errorf("abrir carteira: %d %v %v", st, body, err)
		}
		ws[i].id, _ = body["id"].(string)
	}
	hot := ws[0]

	before := scrapeAll(c.apis)

	// amostragem do atraso da outbox durante a medição
	var (
		lagMax, lagSum, pendingMax float64
		lagN                       int
		sampling                   atomic.Bool
		samplerDone                = make(chan struct{})
	)
	sampling.Store(true)
	go func() {
		defer close(samplerDone)
		for sampling.Load() {
			if s, err := scrapeMetrics(c.apis[0]); err == nil { // gauges globais (lidos do banco)
				lag, pend := s["outbox_lag_seconds"], s["outbox_pending_events"]
				lagMax, pendingMax = math.Max(lagMax, lag), math.Max(pendingMax, pend)
				lagSum += lag
				lagN++
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	var measuring atomic.Bool
	deadline := time.Now().Add(c.warmup + c.duration)
	time.AfterFunc(c.warmup, func() { measuring.Store(true) })

	// taxa fixa (carga aberta): um "bilhete" por requisição, emitido no ritmo
	// pedido; sem -rate, cada cliente dispara assim que recebe a resposta
	var tickets chan struct{}
	if c.rate > 0 {
		tickets = make(chan struct{}, c.concurrency)
		go func() {
			interval := time.Duration(float64(time.Second) / c.rate)
			next := time.Now()
			for time.Now().Before(deadline) {
				next = next.Add(interval)
				time.Sleep(time.Until(next))
				select {
				case tickets <- struct{}{}:
				default: // clientes todos ocupados: a requisição é perdida (conta como atraso do gerador)
				}
			}
			close(tickets)
		}()
	}

	workers := make([]*worker, c.concurrency)
	var wg sync.WaitGroup
	var measureStart, measureEnd time.Time
	measureStart = time.Now().Add(c.warmup)
	for i := range workers {
		w := &worker{}
		workers[i] = w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if tickets != nil {
					if _, ok := <-tickets; !ok {
						return
					}
				}
				var o op
				kind := ""
				r := mrand.Float64()
				switch {
				case r < c.replayFraction && len(w.done) > 0:
					o, kind = w.done[mrand.IntN(len(w.done))], "REPLAY"
				default:
					target := ws[1+mrand.IntN(len(ws)-1)]
					if mrand.Float64() < c.hotFraction {
						target = hot
					}
					switch k := mrand.Float64(); {
					case k < 0.70:
						o, kind = newOp(target, "BET", "1.00"), "BET"
					case k < 0.95:
						o, kind = newOp(target, "WIN", "1.50"), "WIN"
					default:
						o, kind = newOp(target, "LOSS", "0.00"), "LOSS"
					}
				}
				start := time.Now()
				st, body, err := do(ctx, "POST", api()+"/wagering/transactions", tk.get(o.provider), o.body,
					"Idempotency-Key", o.key)
				lat := time.Since(start)
				out := classify(st, body, err)
				if out == "created" && len(w.done) < 1000 {
					w.done = append(w.done, o)
				}
				if measuring.Load() {
					w.samples = append(w.samples, sample{kind: kind, outcome: out, latency: lat})
				}
			}
		}()
	}
	wg.Wait()
	measureEnd = time.Now()
	sampling.Store(false)
	<-samplerDone
	elapsed := measureEnd.Sub(measureStart)

	// outbox: quanto tempo para esvaziar depois da carga
	drainStart := time.Now()
	var drained time.Duration = -1
	for time.Since(drainStart) < 2*time.Minute {
		if s, err := scrapeMetrics(c.apis[0]); err == nil && s["outbox_pending_events"] == 0 {
			drained = time.Since(drainStart)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// reconciliação de todas as carteiras
	consistent, divergent := 0, 0
	for _, w := range ws {
		st, body, err := do(ctx, "POST", api()+"/wallets/"+w.id+"/reconciliation", tk.get("wallet-service"), nil)
		if err == nil && st == http.StatusOK && body["consistent"] == true {
			consistent++
		} else {
			divergent++
		}
	}

	after := scrapeAll(c.apis)
	delta := func(prefix string) float64 { return sum(after, prefix) - sum(before, prefix) }

	var all []sample
	for _, w := range workers {
		all = append(all, w.samples...)
	}
	rep := report{
		cfg: c, elapsed: elapsed, samples: all,
		lockConflicts: delta(`wallet_lock_conflicts_total{source="http"}`),
		retries:       delta(`transient_retries_total{source="http"}`),
		lagMax:        lagMax, pendingMax: pendingMax, drained: drained,
		consistent: consistent, divergent: divergent,
	}
	if lagN > 0 {
		rep.lagAvg = lagSum / float64(lagN)
	}
	text := rep.markdown()
	fmt.Println(text)
	if c.out != "" {
		if err := os.WriteFile(c.out, []byte(text), 0o644); err != nil {
			return err
		}
	}
	if divergent > 0 {
		return errors.New("há carteiras divergentes")
	}
	return nil
}

// ------------------------------------------------------------------ relatório

type report struct {
	cfg                        config
	elapsed                    time.Duration
	samples                    []sample
	lockConflicts, retries     float64
	lagMax, lagAvg, pendingMax float64
	drained                    time.Duration
	consistent, divergent      int
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000) }

func (r report) markdown() string {
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	label := r.cfg.label
	if label == "" {
		label = fmt.Sprintf("%d instância(s), %d clientes", len(r.cfg.apis), r.cfg.concurrency)
	}
	p("## %s\n\n", label)
	p("| Parâmetro | Valor |\n|---|---|\n")
	p("| instâncias | %d (%s) |\n", len(r.cfg.apis), strings.Join(r.cfg.apis, ", "))
	p("| clientes simultâneos | %d |\n", r.cfg.concurrency)
	if r.cfg.rate > 0 {
		p("| taxa pedida | %.0f req/s (carga aberta, ritmo fixo) |\n", r.cfg.rate)
	} else {
		p("| taxa pedida | sem limite (cada cliente envia assim que recebe a resposta: mede a capacidade) |\n")
	}
	p("| duração medida | %s (após %s de aquecimento) |\n", r.elapsed.Round(time.Second), r.cfg.warmup)
	p("| carteiras | %d (%.0f%% das operações numa carteira disputada) |\n", r.cfg.wallets, r.cfg.hotFraction*100)
	p("| mistura | 70%% BET, 25%% WIN, 5%% LOSS; %.0f%% de reenvios |\n", r.cfg.replayFraction*100)
	p("| máquina do gerador | %s/%s, %d CPUs, %s |\n\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())

	total := len(r.samples)
	byOutcome := map[string]int{}
	var lat []time.Duration
	for _, s := range r.samples {
		byOutcome[s.outcome]++
		lat = append(lat, s.latency)
	}
	slices.Sort(lat)
	ok := byOutcome["created"] + byOutcome["replay"] + byOutcome["rejected"]
	errs := total - ok
	p("| Resultado | Valor |\n|---|---|\n")
	p("| requisições | %d |\n", total)
	p("| **throughput** | **%.0f req/s** |\n", float64(total)/r.elapsed.Seconds())
	p("| **latência p50 / p95 / p99** | **%s / %s / %s ms** |\n", ms(percentile(lat, 50)), ms(percentile(lat, 95)), ms(percentile(lat, 99)))
	p("| latência máxima | %s ms |\n", ms(percentile(lat, 100)))
	p("| **erros** (503, 409, transporte, outros) | **%d (%.2f%%)** |\n", errs, 100*float64(errs)/math.Max(1, float64(total)))
	p("| **conflitos de lock** (`wallet_lock_conflicts_total`) | **%.0f** |\n", r.lockConflicts)
	p("| novas tentativas automáticas | %.0f |\n", r.retries)
	p("| **atraso da outbox** (`outbox_lag_seconds`) médio / máximo | **%.2f s / %.2f s** |\n", r.lagAvg, r.lagMax)
	p("| eventos pendentes na outbox (máximo) | %.0f |\n", r.pendingMax)
	if r.drained >= 0 {
		p("| outbox vazia depois da carga em | %s |\n", r.drained.Round(100*time.Millisecond))
	} else {
		p("| outbox vazia depois da carga em | não esvaziou em 2 min |\n")
	}
	p("| reconciliação | %d consistentes, %d divergentes |\n\n", r.consistent, r.divergent)

	p("| Desfecho | Quantidade |\n|---|---|\n")
	keys := make([]string, 0, len(byOutcome))
	for k := range byOutcome {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		p("| %s | %d |\n", k, byOutcome[k])
	}
	p("\n| Tipo | Requisições | p50 | p95 | p99 (ms) |\n|---|---|---|---|---|\n")
	byKind := map[string][]time.Duration{}
	for _, s := range r.samples {
		byKind[s.kind] = append(byKind[s.kind], s.latency)
	}
	for _, k := range []string{"BET", "WIN", "LOSS", "REPLAY"} {
		l := byKind[k]
		slices.Sort(l)
		p("| %s | %d | %s | %s | %s |\n", k, len(l), ms(percentile(l, 50)), ms(percentile(l, 95)), ms(percentile(l, 99)))
	}
	return b.String()
}
