package main

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/repository/postgres"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/seed"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/testutil/pgtest"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func key(seed byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// env arma la lectura del entorno del seed.
func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func devEnv(dsn string) map[string]string {
	return map[string]string{
		"APP_ENV":                "development",
		"DATABASE_URL":           dsn,
		"RABBITMQ_URL":           "amqp://guest:guest@127.0.0.1:1/",
		"BENEFITS_CODE_AES_KEY":  key(1),
		"BENEFITS_CODE_HMAC_KEY": key(100),
	}
}

func TestElSeedSoloCorreConAppEnvDevelopment(t *testing.T) {
	for nombre, appEnv := range map[string]string{"sin definir": "", "production": "production", "mayúsculas": "Development", "staging": "staging"} {
		t.Run(nombre, func(t *testing.T) {
			values := devEnv("postgres://nadie:nada@127.0.0.1:1/nada?sslmode=disable")
			values["APP_ENV"] = appEnv

			err := run(context.Background(), env(values), quietLogger())

			if err != errNotDevelopment {
				t.Fatalf("error = %v, esperaba errNotDevelopment sin tocar la base", err)
			}
		})
	}
}

func TestElSeedCargaLosDatosDePruebaYEsIdempotente(t *testing.T) {
	dsn := pgtest.NewSchemaDSN(t)
	values := devEnv(dsn)

	for i := 0; i < 2; i++ {
		if err := run(context.Background(), env(values), quietLogger()); err != nil {
			t.Fatalf("ejecución %d: %v", i+1, err)
		}
	}

	db, err := postgres.Connect(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	repo := postgres.NewRepository(db)
	summaries, err := repo.ListCampaigns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("campañas = %d, esperaba 2 aun tras correr el seed dos veces", len(summaries))
	}
	byID := map[string]int64{}
	for _, s := range summaries {
		byID[s.ID.String()] = s.Stats.Available
	}
	if byID[seed.ActiveCampaignID.String()] != seed.ActiveCodeCount {
		t.Errorf("la campaña vigente tiene %d códigos disponibles, esperaba %d", byID[seed.ActiveCampaignID.String()], seed.ActiveCodeCount)
	}

	visibles, err := repo.ListLive(context.Background(), seed.ActiveCampaignID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(visibles) != 1 || visibles[0].ID != seed.ActiveCampaignID || visibles[0].Title != "Aplicación de prueba" {
		t.Errorf("visibles = %+v; con el seed cargado solo se muestra la campaña vigente, no la vencida", visibles)
	}
}

func TestElSeedFallaSiLaConfiguracionEsInvalida(t *testing.T) {
	values := devEnv("")

	err := run(context.Background(), env(values), quietLogger())

	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("error = %v, esperaba que nombrara DATABASE_URL", err)
	}
}

func TestElSeedFallaSiLaBaseNoEstaDisponible(t *testing.T) {
	values := devEnv("postgres://nadie:nada@127.0.0.1:1/nada?sslmode=disable&connect_timeout=1")

	if err := run(context.Background(), env(values), quietLogger()); err == nil {
		t.Fatal("esperaba un error sin base de datos")
	}
}

func TestExecuteDevuelveElCodigoDeSalida(t *testing.T) {
	if code := execute(context.Background(), env(map[string]string{"APP_ENV": "production"}), quietLogger()); code != exitError {
		t.Errorf("con APP_ENV=production el código de salida = %d, esperaba %d", code, exitError)
	}

	if code := execute(context.Background(), env(devEnv(pgtest.NewSchemaDSN(t))), quietLogger()); code != exitOK {
		t.Errorf("con el entorno de desarrollo el código de salida = %d, esperaba %d", code, exitOK)
	}
}
