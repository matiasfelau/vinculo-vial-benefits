package http

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

// Códigos de error del contrato. Viajan junto con el mensaje: { "error", "code" }.
const (
	codeUnauthorized          = "UNAUTHORIZED"
	codeBenefitNotFound       = "BENEFIT_NOT_FOUND"
	codeNoCodesLeft           = "NO_CODES_LEFT"
	codeDependencyUnavailable = "DEPENDENCY_UNAVAILABLE"
	codeInternal              = "INTERNAL_ERROR"
	codeNotFound              = "NOT_FOUND"
	codeMethodNotAllowed      = "METHOD_NOT_ALLOWED"

	codeCampaignNotFound   = "CAMPAIGN_NOT_FOUND"
	codeInvalidBody        = "INVALID_BODY"
	codeInvalidBanner      = "INVALID_BANNER"
	codeBannerTooLarge     = "BANNER_TOO_LARGE"
	codeTooManyLines       = "TOO_MANY_LINES"
	codePayloadTooLarge    = "PAYLOAD_TOO_LARGE"
	codeUnsupportedContent = "UNSUPPORTED_MEDIA_TYPE"
)

// retryAfterSeconds es el valor del encabezado Retry-After de los 503: cuánto espera el
// cliente antes de reintentar si la base no respondió.
const retryAfterSeconds = "5"

// errorBody es el formato de error uniforme: { "error", "code" }.
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func respondError(c *gin.Context, status int, code, message string) {
	c.JSON(status, errorBody{Error: message, Code: code})
}

// respondInternal responde el 500 genérico y deja la causa en el log, sin ningún dato
// del cuerpo de la petición.
func respondInternal(c *gin.Context, logger *slog.Logger, err error) {
	logger.ErrorContext(c.Request.Context(), "error interno atendiendo la petición",
		slog.String("request_id", requestIDOf(c)),
		slog.String("route", c.FullPath()),
		slog.String("error", err.Error()),
	)
	respondError(c, http.StatusInternalServerError, codeInternal, "error interno")
}

// respondUnavailable responde el 503 de una dependencia caída, con Retry-After.
func respondUnavailable(c *gin.Context, logger *slog.Logger, err error) {
	logger.WarnContext(c.Request.Context(), "dependencia no disponible",
		slog.String("request_id", requestIDOf(c)),
		slog.String("route", c.FullPath()),
		slog.String("error", err.Error()),
	)
	c.Header("Retry-After", retryAfterSeconds)
	respondError(c, http.StatusServiceUnavailable, codeDependencyUnavailable, "servicio no disponible, reintente en unos segundos")
}

// domainMessage devuelve el texto de un error de dominio sin su prefijo de paquete. Solo
// se usa con errores cuyo texto describe un campo y nunca su valor.
func domainMessage(err error) string {
	message := err.Error()
	if i := strings.Index(message, "domain: "); i >= 0 {
		message = message[i+len("domain: "):]
	}
	return message
}

// isUnavailable informa si err es una indisponibilidad de la base.
func isUnavailable(err error) bool {
	return errors.Is(err, domain.ErrRepositoryUnavailable)
}
