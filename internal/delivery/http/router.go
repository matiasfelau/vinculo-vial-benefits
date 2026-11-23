package http

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// maxPublicBodyBytes acota el cuerpo de las rutas públicas: ninguna lo usa.
const maxPublicBodyBytes = 1 << 10

// Pingable define el contrato para verificar la disponibilidad de una dependencia.
type Pingable interface {
	Ping(ctx context.Context) error
}

// RouterConfig son las dependencias del router público.
type RouterConfig struct {
	Benefits BenefitService
	DB       Pingable
	// Deps son las dependencias que /health/deps reporta, por ejemplo el consumidor de
	// RabbitMQ.
	Deps   []DependencyCheck
	Logger *slog.Logger
}

// NewRouter arma el router público. Kong lo alcanza con jwt y token-offloading, que
// inyecta X-User-Id; el servicio igual lo exige en cada ruta de /api/v1.
func NewRouter(cfg RouterConfig) *gin.Engine {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false
	engine.HandleMethodNotAllowed = true
	// Sin proxies de confianza: el servicio nunca usa la IP del cliente.
	_ = engine.SetTrustedProxies(nil)

	engine.Use(requestID(), accessLog(logger), recovery(logger), securityHeaders(), limitBody(maxPublicBodyBytes))

	engine.GET("/health", healthCheck)
	engine.GET("/ready", readyCheck(cfg.DB, logger))
	engine.GET("/health/deps", depsHealth(cfg.Deps...))

	handler := newBenefitHandler(cfg.Benefits, logger)
	api := engine.Group("/api/v1", noStore(), requireUser(logger))
	api.GET("/benefits", handler.List)
	api.GET("/benefits/:id/banner", handler.Banner)
	api.POST("/benefits/:id/claim", handler.Claim)

	engine.NoRoute(notFound)
	engine.NoMethod(methodNotAllowed)
	return engine
}

func notFound(c *gin.Context) {
	respondError(c, http.StatusNotFound, codeNotFound, "recurso no encontrado")
}

func methodNotAllowed(c *gin.Context) {
	respondError(c, http.StatusMethodNotAllowed, codeMethodNotAllowed, "método no permitido")
}

// limitBody acota el cuerpo que se está dispuesto a leer.
func limitBody(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

func healthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func readyCheck(db Pingable, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := db.Ping(c.Request.Context()); err != nil {
			logger.WarnContext(c.Request.Context(), "base de datos inalcanzable", slog.String("error", err.Error()))
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "detail": "base de datos inalcanzable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

const (
	depUp   = "up"
	depDown = "down"
)

// DependencyCheck es una dependencia que /health/deps reporta bajo su nombre.
type DependencyCheck struct {
	Name    string
	Healthy func() bool
}

// depsHealth expone GET /health/deps: 200 con status "ok" si todas las dependencias
// están arriba, y 503 con status "degraded" si no. No hace llamadas de red y no interviene
// en /ready: el balanceo de Kong depende solo de la base propia.
func depsHealth(checks ...DependencyCheck) gin.HandlerFunc {
	return func(c *gin.Context) {
		deps := gin.H{}
		var down []string
		for _, check := range checks {
			if check.Healthy() {
				deps[check.Name] = depUp
				continue
			}
			deps[check.Name] = depDown
			down = append(down, check.Name+" no disponible")
		}

		body := gin.H{"status": "ok", "deps": deps}
		if len(down) > 0 {
			body["status"] = "degraded"
			body["error"] = strings.Join(down, "; ")
			c.JSON(http.StatusServiceUnavailable, body)
			return
		}
		c.JSON(http.StatusOK, body)
	}
}
