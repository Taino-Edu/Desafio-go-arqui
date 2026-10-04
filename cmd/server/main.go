// Comando server sobe a API HTTP de carteiras.
//
//	server              inicia o serviço (lê a configuração do ambiente)
//	server healthcheck  consulta /health/live local; usado pelo HEALTHCHECK
//	                    do container, que não tem curl
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"go.uber.org/fx"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/config"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/fxapp"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		os.Exit(2)
	}
	// Run bloqueia até SIGINT/SIGTERM e então executa os OnStop em ordem
	// inversa, dentro de SHUTDOWN_TIMEOUT.
	fx.New(fxapp.New(cfg)).Run()
}

func healthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/health/live", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
