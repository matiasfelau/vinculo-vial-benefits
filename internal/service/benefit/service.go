// Package benefit implementa los casos de uso públicos: listar las campañas vigentes de
// un usuario, pedir un código y servir el banner.
package benefit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

// codeDecryptor descifra un código guardado. La implementa crypto/codes.
type codeDecryptor interface {
	Decrypt(encoded string) (string, error)
}

// Service reúne los casos de uso públicos.
type Service struct {
	repo  domain.BenefitRepository
	codes codeDecryptor
	now   func() time.Time
}

// View es una campaña tal como se le muestra a un usuario. Code es nil mientras el
// usuario no lo pidió.
type View struct {
	ID          uuid.UUID
	PartnerName string
	Title       string
	Description string
	Terms       string
	OpenURL     string
	EndsAt      time.Time
	Code        *string
}

// NewService construye el servicio. now es el reloj; nil usa time.Now.
func NewService(repo domain.BenefitRepository, codes codeDecryptor, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, codes: codes, now: now}
}

// List devuelve las campañas visibles para el usuario, ordenadas por ends_at, con su
// código descifrado cuando ya lo pidió.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]View, error) {
	benefits, err := s.repo.ListLive(ctx, userID, s.now())
	if err != nil {
		return nil, fmt.Errorf("no se pudieron listar las campañas: %w", err)
	}

	views := make([]View, 0, len(benefits))
	for _, b := range benefits {
		view := View{
			ID: b.ID, PartnerName: b.PartnerName, Title: b.Title, Description: b.Description,
			Terms: b.Terms, OpenURL: b.OpenURL, EndsAt: b.EndsAt,
		}
		if b.CodeEnc != "" {
			code, err := s.codes.Decrypt(b.CodeEnc)
			if err != nil {
				return nil, fmt.Errorf("no se pudo descifrar el código de la campaña %s: %w", b.ID, err)
			}
			view.Code = &code
		}
		views = append(views, view)
	}
	return views, nil
}

// Claim entrega al usuario su código de la campaña. Es idempotente: pedirlo de nuevo
// devuelve el mismo. Devuelve domain.ErrBenefitNotFound si la campaña no existe o no
// está vigente, y domain.ErrNoCodesLeft si el pool está agotado.
func (s *Service) Claim(ctx context.Context, campaignID, userID uuid.UUID) (string, error) {
	codeEnc, err := s.repo.Claim(ctx, campaignID, userID, s.now())
	if err != nil {
		return "", fmt.Errorf("no se pudo asignar el código: %w", err)
	}

	code, err := s.codes.Decrypt(codeEnc)
	if err != nil {
		return "", fmt.Errorf("no se pudo descifrar el código asignado de la campaña %s: %w", campaignID, err)
	}
	return code, nil
}

// Banner devuelve el banner de una campaña vigente. Si ifNoneMatch coincide con su
// ETag, devuelve notModified en true y sin los bytes de la imagen.
func (s *Service) Banner(ctx context.Context, campaignID uuid.UUID, ifNoneMatch string) (banner domain.Banner, notModified bool, err error) {
	now := s.now()

	meta, err := s.repo.BannerMeta(ctx, campaignID, now)
	if err != nil {
		return domain.Banner{}, false, fmt.Errorf("no se pudo leer el banner: %w", err)
	}
	if ETagMatches(ifNoneMatch, meta.ETag) {
		return meta, true, nil
	}

	full, err := s.repo.BannerData(ctx, campaignID, now)
	if err != nil {
		return domain.Banner{}, false, fmt.Errorf("no se pudo leer el banner: %w", err)
	}
	return full, false, nil
}

// ETagMatches informa si el valor de If-None-Match incluye etag, sin comillas. Acepta
// una lista, la forma débil (W/"...") y el comodín "*".
func ETagMatches(ifNoneMatch, etag string) bool {
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		candidate = strings.TrimPrefix(candidate, "W/")
		if strings.Trim(candidate, `"`) == etag && etag != "" {
			return true
		}
	}
	return false
}
