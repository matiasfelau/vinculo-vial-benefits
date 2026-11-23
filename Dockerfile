## Etapa de compilación
FROM golang:1.27-alpine AS builder

RUN apk add --no-cache ca-certificates

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/benefits ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/seed ./cmd/seed

## Imagen del seed de desarrollo (docker compose --profile seed run --rm ...).
## No es la imagen que se publica: la publicada es la última etapa de este archivo.
FROM scratch AS seed

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /build/seed /seed

# Usuario sin privilegios (UID numérico: la imagen scratch no tiene /etc/passwd)
USER 65532:65532

ENTRYPOINT ["/seed"]

## Etapa final: imagen mínima sin shell ni herramientas adicionales
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /build/benefits /benefits

# Usuario sin privilegios (UID numérico: la imagen scratch no tiene /etc/passwd)
USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/benefits"]
