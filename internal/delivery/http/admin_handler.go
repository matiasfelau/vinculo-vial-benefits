package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/service/campaign"
)

const (
	// maxJSONBodyBytes acota el JSON de un parche: sus campos de texto suman hasta unos
	// 3 KiB, que en UTF-8 pueden ser hasta 12 KiB.
	maxJSONBodyBytes = 16 << 10
	// maxCampaignPartBytes acota la parte "campaign" del multipart de alta.
	maxCampaignPartBytes = 16 << 10
	// multipartOverheadBytes es el margen para los delimitadores y encabezados del
	// multipart sobre sus dos partes.
	multipartOverheadBytes = 4 << 10
	// maxCSVBodyBytes acota un CSV de códigos o de canjes: el máximo de líneas por el
	// largo máximo de un código, más el salto de línea (con retorno de carro).
	maxCSVBodyBytes = domain.MaxCSVLines * (domain.MaxCodeLength + 2)

	mediaTypeCSV = "text/csv"

	partCampaign = "campaign"
	partBanner   = "banner"
)

// CampaignService es el caso de uso de administración que necesitan los handlers,
// declarado del lado del consumidor para probarlos con un doble.
type CampaignService interface {
	Create(ctx context.Context, in campaign.CreateInput) (uuid.UUID, error)
	List(ctx context.Context) ([]domain.CampaignSummary, error)
	Patch(ctx context.Context, id uuid.UUID, patch domain.CampaignPatch) error
	ReplaceBanner(ctx context.Context, id uuid.UUID, data []byte) error
	UploadCodes(ctx context.Context, campaignID uuid.UUID, csv string) (domain.UploadResult, error)
	ReportRedemptions(ctx context.Context, campaignID uuid.UUID, csv string) (domain.RedemptionResult, error)
	Stats(ctx context.Context, campaignID uuid.UUID) (domain.CampaignStats, error)
}

type adminHandler struct {
	service        CampaignService
	bannerMaxBytes int64
	logger         *slog.Logger
}

func newAdminHandler(service CampaignService, bannerMaxBytes int64, logger *slog.Logger) *adminHandler {
	return &adminHandler{service: service, bannerMaxBytes: bannerMaxBytes, logger: logger}
}

// campaignFields son los campos editables de una campaña nueva (parte "campaign").
type campaignFields struct {
	PartnerName string    `json:"partner_name"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Terms       string    `json:"terms"`
	OpenURL     string    `json:"open_url"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	Active      bool      `json:"active"`
}

// patchFields es el cuerpo parcial de PATCH: un campo ausente no se toca.
type patchFields struct {
	Title       *string    `json:"title"`
	Description *string    `json:"description"`
	Terms       *string    `json:"terms"`
	OpenURL     *string    `json:"open_url"`
	StartsAt    *time.Time `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at"`
	Active      *bool      `json:"active"`
}

type statsBody struct {
	Available int64 `json:"available"`
	Assigned  int64 `json:"assigned"`
	Burned    int64 `json:"burned"`
	Redeemed  int64 `json:"redeemed"`
}

func toStatsBody(s domain.CampaignStats) statsBody {
	return statsBody{Available: s.Available, Assigned: s.Assigned, Burned: s.Burned, Redeemed: s.Redeemed}
}

type campaignListItem struct {
	ID          string    `json:"id"`
	PartnerName string    `json:"partner_name"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Terms       string    `json:"terms"`
	OpenURL     string    `json:"open_url"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Stats       statsBody `json:"stats"`
}

// List implementa GET /admin/v1/campaigns.
func (h *adminHandler) List(c *gin.Context) {
	summaries, err := h.service.List(c.Request.Context())
	if err != nil {
		h.respondFailure(c, err)
		return
	}

	items := make([]campaignListItem, 0, len(summaries))
	for _, s := range summaries {
		items = append(items, campaignListItem{
			ID: s.ID.String(), PartnerName: s.PartnerName, Title: s.Title, Description: s.Description,
			Terms: s.Terms, OpenURL: s.OpenURL, StartsAt: s.StartsAt.UTC(), EndsAt: s.EndsAt.UTC(),
			Active: s.Active, CreatedAt: s.CreatedAt.UTC(), UpdatedAt: s.UpdatedAt.UTC(), Stats: toStatsBody(s.Stats),
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// Create implementa POST /admin/v1/campaigns (multipart: partes "campaign" y "banner").
func (h *adminHandler) Create(c *gin.Context) {
	limit := h.bannerMaxBytes + maxCampaignPartBytes + multipartOverheadBytes
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)

	reader, err := c.Request.MultipartReader()
	if err != nil {
		respondError(c, http.StatusBadRequest, codeInvalidBody, "se esperaba un cuerpo multipart con las partes campaign y banner")
		return
	}

	var fieldsRaw, bannerData []byte
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			h.respondReadFailure(c, err)
			return
		}

		switch part.FormName() {
		case partCampaign:
			if fieldsRaw != nil {
				respondError(c, http.StatusBadRequest, codeInvalidBody, "la parte campaign está repetida")
				return
			}
			fieldsRaw, err = readPart(part, maxCampaignPartBytes)
		case partBanner:
			if bannerData != nil {
				respondError(c, http.StatusBadRequest, codeInvalidBody, "la parte banner está repetida")
				return
			}
			bannerData, err = readPart(part, h.bannerMaxBytes)
			if errors.Is(err, errPartTooLarge) {
				respondError(c, http.StatusRequestEntityTooLarge, codeBannerTooLarge, "el banner supera el tamaño máximo")
				return
			}
		default:
			respondError(c, http.StatusBadRequest, codeInvalidBody, "parte desconocida en el multipart")
			return
		}
		if err != nil {
			h.respondReadFailure(c, err)
			return
		}
	}

	if fieldsRaw == nil || bannerData == nil {
		respondError(c, http.StatusBadRequest, codeInvalidBody, "faltan las partes campaign y banner")
		return
	}

	var fields campaignFields
	if err := decodeStrict(fieldsRaw, &fields); err != nil {
		respondError(c, http.StatusBadRequest, codeInvalidBody, "la parte campaign no es un JSON válido con los campos esperados")
		return
	}

	id, err := h.service.Create(c.Request.Context(), campaign.CreateInput{
		PartnerName: fields.PartnerName, Title: fields.Title, Description: fields.Description, Terms: fields.Terms,
		OpenURL: fields.OpenURL, StartsAt: fields.StartsAt, EndsAt: fields.EndsAt, Active: fields.Active, Banner: bannerData,
	})
	if err != nil {
		h.respondFailure(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": id.String()})
}

// Patch implementa PATCH /admin/v1/campaigns/{id}.
func (h *adminHandler) Patch(c *gin.Context) {
	id, ok := parseCampaignParam(c)
	if !ok {
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONBodyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		h.respondReadFailure(c, err)
		return
	}

	var fields patchFields
	if err := decodeStrict(raw, &fields); err != nil {
		respondError(c, http.StatusBadRequest, codeInvalidBody, "el cuerpo no es un JSON válido con los campos editables")
		return
	}

	err = h.service.Patch(c.Request.Context(), id, domain.CampaignPatch{
		Title: fields.Title, Description: fields.Description, Terms: fields.Terms, OpenURL: fields.OpenURL,
		StartsAt: fields.StartsAt, EndsAt: fields.EndsAt, Active: fields.Active,
	})
	if err != nil {
		h.respondFailure(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ReplaceBanner implementa PUT /admin/v1/campaigns/{id}/banner: el cuerpo es la imagen.
func (h *adminHandler) ReplaceBanner(c *gin.Context) {
	id, ok := parseCampaignParam(c)
	if !ok {
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.bannerMaxBytes)
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		h.respondReadFailure(c, err)
		return
	}

	if err := h.service.ReplaceBanner(c.Request.Context(), id, data); err != nil {
		h.respondFailure(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type uploadResponse struct {
	Inserted   int `json:"inserted"`
	Duplicates int `json:"duplicates"`
	Invalid    int `json:"invalid"`
}

// UploadCodes implementa POST /admin/v1/campaigns/{id}/codes (text/csv).
func (h *adminHandler) UploadCodes(c *gin.Context) {
	id, csv, ok := h.readCSV(c)
	if !ok {
		return
	}

	result, err := h.service.UploadCodes(c.Request.Context(), id, csv)
	if err != nil {
		h.respondFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, uploadResponse{Inserted: result.Inserted, Duplicates: result.Duplicates, Invalid: result.Invalid})
}

type redemptionResponse struct {
	Matched int `json:"matched"`
	Unknown int `json:"unknown"`
}

// ReportRedemptions implementa POST /admin/v1/campaigns/{id}/redemptions (text/csv).
func (h *adminHandler) ReportRedemptions(c *gin.Context) {
	id, csv, ok := h.readCSV(c)
	if !ok {
		return
	}

	result, err := h.service.ReportRedemptions(c.Request.Context(), id, csv)
	if err != nil {
		h.respondFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, redemptionResponse{Matched: result.Matched, Unknown: result.Unknown})
}

// Stats implementa GET /admin/v1/campaigns/{id}/stats.
func (h *adminHandler) Stats(c *gin.Context) {
	id, ok := parseCampaignParam(c)
	if !ok {
		return
	}

	stats, err := h.service.Stats(c.Request.Context(), id)
	if err != nil {
		h.respondFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, toStatsBody(stats))
}

// readCSV valida el id y el Content-Type y lee el cuerpo de un CSV, acotado. Si algo
// falla, ya respondió y devuelve ok en false.
func (h *adminHandler) readCSV(c *gin.Context) (id uuid.UUID, csv string, ok bool) {
	id, ok = parseCampaignParam(c)
	if !ok {
		return uuid.Nil, "", false
	}

	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != mediaTypeCSV {
		respondError(c, http.StatusUnsupportedMediaType, codeUnsupportedContent, "el Content-Type debe ser text/csv")
		return uuid.Nil, "", false
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxCSVBodyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		h.respondReadFailure(c, err)
		return uuid.Nil, "", false
	}
	return id, string(raw), true
}

// parseCampaignParam interpreta el id de la ruta. Un id que no es un UUID no puede
// existir: responde 404 y devuelve ok en false.
func parseCampaignParam(c *gin.Context) (uuid.UUID, bool) {
	id, ok := parseCampaignID(c.Param("id"))
	if !ok {
		respondError(c, http.StatusNotFound, codeCampaignNotFound, "la campaña no existe")
		return uuid.Nil, false
	}
	return id, true
}

// errPartTooLarge indica que una parte del multipart supera su tope.
var errPartTooLarge = errors.New("http: parte del multipart demasiado grande")

// readPart lee una parte del multipart hasta max bytes; si trae más, devuelve
// errPartTooLarge. Siempre devuelve un slice no nil si no hay error, aunque esté vacío.
func readPart(part *multipart.Part, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(part, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errPartTooLarge
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

// decodeStrict decodifica un único valor JSON, rechazando campos desconocidos y datos
// sobrantes.
func decodeStrict(raw []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("datos sobrantes después del JSON")
	}
	return nil
}

// respondReadFailure traduce un error al leer el cuerpo: pasarse del tope es 413; lo
// demás, un cuerpo mal formado.
func (h *adminHandler) respondReadFailure(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge), errors.Is(err, errPartTooLarge):
		respondError(c, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "el cuerpo supera el tamaño máximo")
	default:
		respondError(c, http.StatusBadRequest, codeInvalidBody, "no se pudo leer el cuerpo de la petición")
	}
}

// respondFailure traduce un error de los casos de uso al contrato.
func (h *adminHandler) respondFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrCampaignNotFound):
		respondError(c, http.StatusNotFound, codeCampaignNotFound, "la campaña no existe")
	case errors.Is(err, domain.ErrInvalidCampaign):
		respondError(c, http.StatusBadRequest, codeInvalidBody, domainMessage(err))
	case errors.Is(err, domain.ErrInvalidBanner):
		respondError(c, http.StatusBadRequest, codeInvalidBanner, "el banner debe ser una imagen PNG, JPEG o WebP")
	case errors.Is(err, domain.ErrBannerTooLarge):
		respondError(c, http.StatusRequestEntityTooLarge, codeBannerTooLarge, "el banner supera el tamaño máximo")
	case errors.Is(err, domain.ErrTooManyLines):
		respondError(c, http.StatusBadRequest, codeTooManyLines, "el CSV supera las 100000 líneas")
	case isUnavailable(err):
		respondUnavailable(c, h.logger, err)
	default:
		respondInternal(c, h.logger, err)
	}
}
