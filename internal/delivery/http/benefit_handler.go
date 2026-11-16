package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/service/benefit"
)

const (
	// bannerCacheControl deja que la app cachee el banner una hora, solo en el cliente.
	bannerCacheControl = "private, max-age=3600"
	// bannerPathPrefix y bannerPathSuffix arman la banner_url de una campaña.
	bannerPathPrefix = "/api/v1/benefits/"
	bannerPathSuffix = "/banner"
)

// BenefitService es el caso de uso público que necesitan los handlers, declarado del
// lado del consumidor para probarlos con un doble.
type BenefitService interface {
	List(ctx context.Context, userID uuid.UUID) ([]benefit.View, error)
	Claim(ctx context.Context, campaignID, userID uuid.UUID) (string, error)
	Banner(ctx context.Context, campaignID uuid.UUID, ifNoneMatch string) (domain.Banner, bool, error)
}

type benefitHandler struct {
	service BenefitService
	logger  *slog.Logger
}

func newBenefitHandler(service BenefitService, logger *slog.Logger) *benefitHandler {
	return &benefitHandler{service: service, logger: logger}
}

// benefitResponse es un elemento de GET /api/v1/benefits. Code es null mientras el
// usuario no pidió su código.
type benefitResponse struct {
	ID          string  `json:"id"`
	PartnerName string  `json:"partner_name"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Terms       string  `json:"terms"`
	OpenURL     string  `json:"open_url"`
	EndsAt      string  `json:"ends_at"`
	BannerURL   string  `json:"banner_url"`
	Code        *string `json:"code"`
}

type claimResponse struct {
	Code string `json:"code"`
}

// List implementa GET /api/v1/benefits.
func (h *benefitHandler) List(c *gin.Context) {
	views, err := h.service.List(c.Request.Context(), userIDOf(c))
	if err != nil {
		h.respondFailure(c, err)
		return
	}

	body := make([]benefitResponse, 0, len(views))
	for _, v := range views {
		body = append(body, benefitResponse{
			ID:          v.ID.String(),
			PartnerName: v.PartnerName,
			Title:       v.Title,
			Description: v.Description,
			Terms:       v.Terms,
			OpenURL:     v.OpenURL,
			EndsAt:      v.EndsAt.UTC().Format(time.RFC3339),
			BannerURL:   bannerPathPrefix + v.ID.String() + bannerPathSuffix,
			Code:        v.Code,
		})
	}
	c.JSON(http.StatusOK, body)
}

// Banner implementa GET /api/v1/benefits/{id}/banner.
func (h *benefitHandler) Banner(c *gin.Context) {
	campaignID, ok := parseCampaignID(c.Param("id"))
	if !ok {
		respondBenefitNotFound(c)
		return
	}

	banner, notModified, err := h.service.Banner(c.Request.Context(), campaignID, c.GetHeader("If-None-Match"))
	if err != nil {
		h.respondFailure(c, err)
		return
	}

	c.Header("ETag", strconv.Quote(banner.ETag))
	c.Header("Cache-Control", bannerCacheControl)
	if notModified {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, banner.ContentType, banner.Data)
}

// Claim implementa POST /api/v1/benefits/{id}/claim.
func (h *benefitHandler) Claim(c *gin.Context) {
	campaignID, ok := parseCampaignID(c.Param("id"))
	if !ok {
		respondBenefitNotFound(c)
		return
	}

	code, err := h.service.Claim(c.Request.Context(), campaignID, userIDOf(c))
	if err != nil {
		h.respondFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, claimResponse{Code: code})
}

// respondFailure traduce un error de los casos de uso al contrato. Es la única capa que
// conoce los códigos HTTP.
func (h *benefitHandler) respondFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrBenefitNotFound):
		respondBenefitNotFound(c)
	case errors.Is(err, domain.ErrNoCodesLeft):
		respondError(c, http.StatusConflict, codeNoCodesLeft, "se agotaron los códigos de esta promoción")
	case isUnavailable(err):
		respondUnavailable(c, h.logger, err)
	default:
		respondInternal(c, h.logger, err)
	}
}

func respondBenefitNotFound(c *gin.Context) {
	respondError(c, http.StatusNotFound, codeBenefitNotFound, "la promoción no existe o ya no está vigente")
}

// parseCampaignID interpreta el id de la ruta. Un id que no es un UUID no puede existir:
// se trata como no encontrado.
func parseCampaignID(raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}
