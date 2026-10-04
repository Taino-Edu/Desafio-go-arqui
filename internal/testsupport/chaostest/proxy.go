// Package chaostest simula falhas de infraestrutura nos testes de
// integração. Proxy fica entre a aplicação e uma dependência real (Postgres,
// LocalStack): Cut derruba todas as conexões e recusa novas, como se a
// dependência tivesse caído; Restore volta ao normal.
package chaostest

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// Proxy é um encaminhador TCP que pode ser cortado.
type Proxy struct {
	ln     net.Listener
	target string
	down   atomic.Bool

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// NewProxy escuta numa porta livre de 127.0.0.1 e encaminha para target
// (host:porta). É fechado no fim do teste.
func NewProxy(t testing.TB, target string) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(p.close)
	return p
}

// Addr devolve host:porta do proxy.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Cut derruba as conexões abertas e passa a recusar as novas (aceita e
// fecha na hora: o cliente vê a falha imediatamente, sem esperar timeout).
func (p *Proxy) Cut() {
	p.down.Store(true)
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
}

// Restore volta a encaminhar conexões novas.
func (p *Proxy) Restore() { p.down.Store(false) }

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return // listener fechado
		}
		if p.down.Load() {
			_ = client.Close()
			continue
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		if !p.track(client, server) {
			continue
		}
		p.wg.Add(2)
		go p.pipe(client, server)
		go p.pipe(server, client)
	}
}

// track registra o par; se um Cut aconteceu entre o Accept e aqui, fecha.
func (p *Proxy) track(a, b net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down.Load() {
		_ = a.Close()
		_ = b.Close()
		return false
	}
	p.conns[a], p.conns[b] = struct{}{}, struct{}{}
	return true
}

func (p *Proxy) pipe(dst, src net.Conn) {
	defer p.wg.Done()
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
	p.mu.Lock()
	delete(p.conns, dst)
	delete(p.conns, src)
	p.mu.Unlock()
}

func (p *Proxy) close() {
	_ = p.ln.Close()
	p.Cut()
	p.wg.Wait()
}
