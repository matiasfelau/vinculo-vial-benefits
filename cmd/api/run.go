package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/crypto/codes"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/delivery/events"
	httpdelivery "github.com/matiasfelau/vinculo-vial-benefits/internal/delivery/http"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/lifecycle"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/repository/postgres"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/service/benefit"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/service/campaign"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/service/userdeleted"
	"github.com/matiasfelau/vinculo-vial-benefits/pkg/config"
)

// Plazos de los *http.Server. Sin ellos, el server de Go no aplica ningún límite de
// lectura ni de escritura: una conexión lenta o maliciosa retendría el handler y su
// goroutine sin plazo.
const (
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 10 * time.Second
	httpWriteTimeout      = 15 * time.Second
	httpIdleTimeout       = 60 * time.Second

	// adminReadTimeout deja leer un CSV de hasta 6,6 MB desde una conexión lenta.
	adminReadTimeout = 60 * time.Second
	// adminWriteMargin se suma a ADMIN_DB_TIMEOUT_MS para el plazo de escritura del
	// servidor de administración: una carga de 100.000 códigos responde recién al terminar
	// su transacción.
	adminWriteMargin = 30 * time.Second

	// shutdownTimeout acota cuánto espera el apagado a que las peticiones en vuelo terminen.
	shutdownTimeout = 10 * time.Second
	// consumerShutdownTimeout acota cuánto espera el apagado al mensaje en curso del
	// consumidor de user.deleted.
	consumerShutdownTimeout = 5 * time.Second
)

// run arma el servicio con cfg y lo sirve hasta que ctx se cancele. Devuelve nil tras un
// apagado ordenado, o el error que impidió arrancar o que cortó un servidor.
func run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
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
	defer func() {
		if err := sqlDB.Close(); err != nil {
			logger.Warn("no se pudo cerrar el pool de conexiones", slog.String("error", err.Error()))
		}
	}()

	if err := postgres.AutoMigrate(db); err != nil {
		return fmt.Errorf("no se pudo migrar el esquema: %w", err)
	}

	repository := postgres.NewRepository(db,
		postgres.WithQueryTimeout(cfg.DBQueryTimeout),
		postgres.WithAdminTimeout(cfg.AdminDBTimeout),
	)

	consumerStatus := &events.ConnectionStatus{}
	servers, err := buildServers(cfg, logger, repository, protector, consumerStatus)
	if err != nil {
		return err
	}

	// El consumidor de user.deleted corre hasta la señal de apagado, con su propia conexión
	// y reconexión: un broker caído no afecta a las rutas HTTP. Al recibir la señal termina
	// el mensaje en curso; el resto vuelve a la cola.
	burnService := userdeleted.NewBurnCodesService(repository)
	consumerCtx, stopConsumer := context.WithCancel(ctx)
	defer stopConsumer()
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		events.RunWithReconnect(consumerCtx, cfg.RabbitMQURL.Reveal(), cfg.UserDeletedQueue, cfg.RabbitMQPrefetch, consumerStatus,
			func(channel events.ConsumerChannel) *events.Consumer {
				deadLetters := events.NewAMQPDeadLetterPublisher(channel, cfg.RabbitMQPublishTimeout)
				handler := events.NewMessageHandler(events.NewUserDeletedProcessor(burnService), deadLetters, cfg.UserDeletedQueue, cfg.MaxRetries)
				return events.NewConsumer(channel, cfg.UserDeletedQueue, handler)
			})
	}()

	serveErr := lifecycle.RunUntilSignal(ctx, servers, shutdownTimeout)
	// Si un servidor falló antes de la señal de apagado, ctx sigue vivo: se detiene el
	// consumidor igual, para no esperarlo hasta su plazo.
	stopConsumer()

	waitCtx, cancel := context.WithTimeout(context.Background(), consumerShutdownTimeout)
	defer cancel()
	select {
	case <-consumerDone:
	case <-waitCtx.Done():
		logger.Warn("el consumidor de user.deleted no terminó a tiempo; el mensaje en vuelo se reentrega al reiniciar")
	}

	if serveErr != nil {
		return fmt.Errorf("error en el servidor HTTP: %w", serveErr)
	}
	return nil
}

// buildServers arma el servidor público y, si hay ADMIN_API_KEY, el de administración.
func buildServers(cfg *config.Config, logger *slog.Logger, repository *postgres.Repository, protector *codes.Protector, consumerStatus *events.ConnectionStatus) ([]lifecycle.Server, error) {
	benefits := benefit.NewService(repository, protector, nil)

	publicRouter := httpdelivery.NewRouter(httpdelivery.RouterConfig{
		Benefits: benefits,
		DB:       repository,
		Deps:     []httpdelivery.DependencyCheck{{Name: "user_deleted_consumer", Healthy: consumerStatus.Connected}},
		Logger:   logger,
	})
	servers := []lifecycle.Server{&http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           publicRouter,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}}
	logger.Info("servidor público iniciado", slog.String("port", cfg.Port))

	if !cfg.AdminEnabled() {
		logger.Warn("ADMIN_API_KEY vacía: el router de administración no arranca")
		return servers, nil
	}

	adminRouter, err := httpdelivery.NewAdminRouter(httpdelivery.AdminRouterConfig{
		Campaigns:      campaign.NewService(repository, protector, cfg.BannerMaxBytes, nil),
		Logger:         logger,
		APIKey:         cfg.AdminAPIKey.Reveal(),
		BannerMaxBytes: cfg.BannerMaxBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("no se pudo armar el router de administración: %w", err)
	}
	servers = append(servers, &http.Server{
		Addr:              ":" + cfg.AdminPort,
		Handler:           adminRouter,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       adminReadTimeout,
		WriteTimeout:      cfg.AdminDBTimeout + adminWriteMargin,
		IdleTimeout:       httpIdleTimeout,
	})
	logger.Info("servidor de administración iniciado", slog.String("port", cfg.AdminPort))
	return servers, nil
}
