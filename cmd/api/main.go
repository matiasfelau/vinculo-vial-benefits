// Command api es el servicio de beneficios de Vinculo Vial: distribuye los códigos de
// descuento de los partners, uno por persona y por campaña.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/matiasfelau/vinculo-vial-benefits/pkg/config"
)

// Códigos de salida del proceso.
const (
	exitOK    = 0
	exitError = 1
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// El apagado ordenado se dispara ante SIGINT o SIGTERM (la señal que envía un
	// orquestador de contenedores): las peticiones en vuelo terminan de atenderse antes de
	// cerrar el proceso.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := execute(ctx, config.Load, logger)
	stop()
	os.Exit(code)
}

// execute carga la configuración, sirve hasta que ctx se cancele y devuelve el código de
// salida del proceso: distinto de cero si la configuración es inválida o el servicio no
// pudo arrancar.
func execute(ctx context.Context, load func() (*config.Config, error), logger *slog.Logger) int {
	cfg, err := load()
	if err != nil {
		logger.Error("no se pudo cargar la configuración", slog.String("error", err.Error()))
		return exitError
	}

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("el servicio terminó con error", slog.String("error", err.Error()))
		return exitError
	}
	return exitOK
}
