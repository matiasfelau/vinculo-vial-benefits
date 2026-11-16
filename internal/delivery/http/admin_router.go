package http

import (
	"errors"
	"log/slog"

	"github.com/gin-gonic/gin"
)

// minAdminAPIKeyLength es el piso de entropía de la clave. pkg/config lo exige igual al
// arrancar; se repite acá para que el router no se pueda armar con una clave débil desde
// otro llamador.
const minAdminAPIKeyLength = 32

// AdminRouterConfig son los parámetros de armado del router de administración.
//
// Se sirve en su propio *http.Server y su propio puerto: Kong no lo expone, y en el
// compose de desarrollo solo se publica en 127.0.0.1. En AWS solo se alcanza desde la
// VPC. APIKey es la autenticación de toda ruta: no hay rol de administrador ni sesión.
type AdminRouterConfig struct {
	Campaigns CampaignService
	Logger    *slog.Logger
	// APIKey es la clave que toda petición debe traer en HeaderAdminAPIKey: al menos 32
	// caracteres.
	APIKey string
	// BannerMaxBytes es el tope del banner, por ruta de carga de imagen.
	BannerMaxBytes int64
}

// NewAdminRouter arma el router de administración.
func NewAdminRouter(cfg AdminRouterConfig) (*gin.Engine, error) {
	if len(cfg.APIKey) < minAdminAPIKeyLength {
		return nil, errors.New("http: la clave de administración (ADMIN_API_KEY) debe tener al menos 32 caracteres")
	}
	if cfg.BannerMaxBytes < 1 {
		return nil, errors.New("http: el tope del banner debe ser mayor a cero")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false
	engine.HandleMethodNotAllowed = true
	if err := engine.SetTrustedProxies(nil); err != nil {
		return nil, err
	}

	engine.Use(requestID(), accessLog(logger), recovery(logger), requireAdminAPIKey(cfg.APIKey, logger), securityHeaders(), noStore())

	handler := newAdminHandler(cfg.Campaigns, cfg.BannerMaxBytes, logger)
	v1 := engine.Group("/admin/v1")
	v1.GET("/campaigns", handler.List)
	v1.POST("/campaigns", handler.Create)
	v1.PATCH("/campaigns/:id", handler.Patch)
	v1.PUT("/campaigns/:id/banner", handler.ReplaceBanner)
	v1.POST("/campaigns/:id/codes", handler.UploadCodes)
	v1.POST("/campaigns/:id/redemptions", handler.ReportRedemptions)
	v1.GET("/campaigns/:id/stats", handler.Stats)

	engine.NoRoute(notFound)
	engine.NoMethod(methodNotAllowed)
	return engine, nil
}
