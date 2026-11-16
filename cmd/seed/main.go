// Command seed carga datos de prueba en la base de beneficios. Solo corre con
// APP_ENV=development: en cualquier otro entorno se niega.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/crypto/codes"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/repository/postgres"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/seed"
	"github.com/matiasfelau/vinculo-vial-benefits/pkg/config"
)

// requiredEnv es el único valor de APP_ENV con el que el seed corre. Se exige explícito:
// con APP_ENV sin definir, el seed no corre aunque la configuración asuma desarrollo.
const requiredEnv = "development"

// seedTimeout acota la carga completa.
const seedTimeout = 2 * time.Minute

var errNotDevelopment = errors.New("el seed solo corre con APP_ENV=development")

// Códigos de salida del proceso.
const (
	exitOK    = 0
	exitError = 1
)

func main() {
	os.Exit(execute(context.Background(), os.Getenv, slog.New(slog.NewJSONHandler(os.Stdout, nil))))
}

// execute corre el seed y devuelve el código de salida del proceso.
func execute(ctx context.Context, getenv func(string) string, logger *slog.Logger) int {
	if err := run(ctx, getenv, logger); err != nil {
		logger.Error("el seed falló", slog.String("error", err.Error()))
		return exitError
	}
	return exitOK
}

// run carga los datos de prueba. getenv es la lectura del entorno, inyectable.
func run(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	if getenv("APP_ENV") != requiredEnv {
		return errNotDevelopment
	}

	cfg, err := config.LoadFrom(getenv)
	if err != nil {
		return fmt.Errorf("no se pudo cargar la configuración: %w", err)
	}

	protector, err := codes.NewProtector(cfg.CodeAESKey.Reveal(), cfg.CodeHMACKey.Reveal())
	if err != nil {
		return fmt.Errorf("no se pudo inicializar la protección de códigos: %w", err)
	}

	db, err := postgres.Connect(cfg.DatabaseURL.Reveal())
	if err != nil {
		return fmt.Errorf("no se pudo conectar a la base de datos: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("no se pudo obtener la conexión subyacente: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	if err := postgres.AutoMigrate(db); err != nil {
		return fmt.Errorf("no se pudo migrar el esquema: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()

	repository := postgres.NewRepository(db, postgres.WithAdminTimeout(seedTimeout))
	if err := seed.Run(ctx, repository, protector, cfg.BannerMaxBytes, time.Now()); err != nil {
		return err
	}

	logger.Info("seed cargado",
		slog.String("campania_vigente", seed.ActiveCampaignID.String()),
		slog.String("campania_vencida", seed.ExpiredCampaignID.String()),
		slog.Int("codigos_vigente", seed.ActiveCodeCount),
	)
	return nil
}
