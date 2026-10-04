# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.27.1-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

# dependências primeiro: camada reaproveitada enquanto go.mod/go.sum não mudam
# extra_ca (opcional): bundle de CAs para builds atrás de proxy que
# inspeciona TLS. Montado só durante o RUN; não fica na imagem.
COPY go.mod go.sum ./
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then export SSL_CERT_FILE=/run/secrets/extra_ca; fi; \
    go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# ---- runtime: imagem mínima, sem shell, usuário sem privilégios ----
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/server /out/migrate /app/
USER 65532:65532
EXPOSE 8080
ENV HTTP_ADDR=:8080
HEALTHCHECK --interval=5s --timeout=3s --retries=10 CMD ["/app/server", "healthcheck"]
ENTRYPOINT ["/app/server"]
