// Package campaign implementa los casos de uso de administración: campañas, banners,
// lotes de códigos y reportes de canje.
package campaign

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/banner"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

// codeProtector cifra los códigos y calcula su HMAC. La implementa crypto/codes.
type codeProtector interface {
	Encrypt(code string) (string, error)
	HMAC(code string) string
}

// Service reúne los casos de uso de administración.
type Service struct {
	repo           domain.CampaignRepository
	codes          codeProtector
	bannerMaxBytes int64
	now            func() time.Time
}

// NewService construye el servicio. bannerMaxBytes es el tope del banner; now es el
// reloj, y nil usa time.Now.
func NewService(repo domain.CampaignRepository, codes codeProtector, bannerMaxBytes int64, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, codes: codes, bannerMaxBytes: bannerMaxBytes, now: now}
}

// CreateInput son los campos editables de una campaña nueva, más su banner.
type CreateInput struct {
	PartnerName string
	Title       string
	Description string
	Terms       string
	OpenURL     string
	StartsAt    time.Time
	EndsAt      time.Time
	Active      bool
	Banner      []byte
}

// Create valida y persiste una campaña con su banner y devuelve su id. Devuelve un error
// que envuelve domain.ErrInvalidCampaign, domain.ErrInvalidBanner o
// domain.ErrBannerTooLarge si la entrada no cumple el contrato; nada se persiste.
func (s *Service) Create(ctx context.Context, in CreateInput) (uuid.UUID, error) {
	now := s.now().UTC()
	campaign := domain.Campaign{
		ID:          uuid.New(),
		PartnerName: strings.TrimSpace(in.PartnerName),
		Title:       strings.TrimSpace(in.Title),
		Description: strings.TrimSpace(in.Description),
		Terms:       strings.TrimSpace(in.Terms),
		OpenURL:     strings.TrimSpace(in.OpenURL),
		StartsAt:    in.StartsAt.UTC(),
		EndsAt:      in.EndsAt.UTC(),
		Active:      in.Active,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := campaign.Validate(); err != nil {
		return uuid.Nil, err
	}

	image, err := banner.Inspect(in.Banner, s.bannerMaxBytes)
	if err != nil {
		return uuid.Nil, err
	}

	if err := s.repo.CreateCampaign(ctx, campaign, image); err != nil {
		return uuid.Nil, fmt.Errorf("no se pudo crear la campaña: %w", err)
	}
	return campaign.ID, nil
}

// List devuelve todas las campañas con sus estadísticas.
func (s *Service) List(ctx context.Context) ([]domain.CampaignSummary, error) {
	summaries, err := s.repo.ListCampaigns(ctx)
	if err != nil {
		return nil, fmt.Errorf("no se pudieron listar las campañas: %w", err)
	}
	return summaries, nil
}

// Patch aplica un cambio parcial. La campaña resultante se valida completa, así que
// un parche no puede dejar ends_at antes de starts_at. Devuelve
// domain.ErrCampaignNotFound si no existe y un error que envuelve
// domain.ErrInvalidCampaign si el resultado no cumple el contrato.
func (s *Service) Patch(ctx context.Context, id uuid.UUID, patch domain.CampaignPatch) error {
	if patch.IsEmpty() {
		return fmt.Errorf("%w: el parche no cambia ningún campo", domain.ErrInvalidCampaign)
	}

	patch = trimPatch(patch)
	err := s.repo.UpdateCampaign(ctx, id, s.now().UTC(), func(current domain.Campaign) (domain.Campaign, error) {
		updated := patch.Apply(current)
		updated.StartsAt = updated.StartsAt.UTC()
		updated.EndsAt = updated.EndsAt.UTC()
		if err := updated.Validate(); err != nil {
			return domain.Campaign{}, err
		}
		return updated, nil
	})
	if err != nil {
		return fmt.Errorf("no se pudo actualizar la campaña: %w", err)
	}
	return nil
}

// trimPatch recorta los espacios de los textos del parche, igual que Create.
func trimPatch(p domain.CampaignPatch) domain.CampaignPatch {
	trim := func(v *string) *string {
		if v == nil {
			return nil
		}
		t := strings.TrimSpace(*v)
		return &t
	}
	p.Title, p.Description, p.Terms, p.OpenURL = trim(p.Title), trim(p.Description), trim(p.Terms), trim(p.OpenURL)
	return p
}

// ReplaceBanner valida y reemplaza el banner de una campaña.
func (s *Service) ReplaceBanner(ctx context.Context, id uuid.UUID, data []byte) error {
	image, err := banner.Inspect(data, s.bannerMaxBytes)
	if err != nil {
		return err
	}
	if err := s.repo.ReplaceBanner(ctx, id, image, s.now().UTC()); err != nil {
		return fmt.Errorf("no se pudo reemplazar el banner: %w", err)
	}
	return nil
}

// UploadCodes carga el lote de códigos de un CSV: un código por línea. Las líneas en
// blanco se ignoran; las que no cumplen el formato se cuentan como inválidas; los
// códigos repetidos en el archivo o ya cargados se cuentan como duplicados. La carga es
// todo o nada. Devuelve domain.ErrTooManyLines si el archivo supera el máximo.
func (s *Service) UploadCodes(ctx context.Context, campaignID uuid.UUID, csv string) (domain.UploadResult, error) {
	lines, err := parseLines(csv)
	if err != nil {
		return domain.UploadResult{}, err
	}

	codes, invalid, duplicates := classify(lines)

	batch := make([]domain.NewCode, 0, len(codes))
	for _, code := range codes {
		enc, err := s.codes.Encrypt(code)
		if err != nil {
			return domain.UploadResult{}, fmt.Errorf("no se pudo cifrar un código: %w", err)
		}
		batch = append(batch, domain.NewCode{Enc: enc, HMAC: s.codes.HMAC(code)})
	}

	inserted, err := s.repo.InsertCodes(ctx, campaignID, batch)
	if err != nil {
		return domain.UploadResult{}, fmt.Errorf("no se pudieron cargar los códigos: %w", err)
	}

	return domain.UploadResult{Inserted: inserted, Duplicates: duplicates + (len(codes) - inserted), Invalid: invalid}, nil
}

// ReportRedemptions marca como canjeados los códigos que reporta el partner: un código
// por línea. matched cuenta los códigos de la campaña, aunque ya estuvieran canjeados;
// unknown, los que no existen en la campaña y las líneas con formato inválido. Un
// código repetido en el reporte cuenta una vez. Devuelve domain.ErrTooManyLines si el
// archivo supera el máximo.
func (s *Service) ReportRedemptions(ctx context.Context, campaignID uuid.UUID, csv string) (domain.RedemptionResult, error) {
	lines, err := parseLines(csv)
	if err != nil {
		return domain.RedemptionResult{}, err
	}

	codes, invalid, _ := classify(lines)

	hmacs := make([]string, 0, len(codes))
	for _, code := range codes {
		hmacs = append(hmacs, s.codes.HMAC(code))
	}

	matched, err := s.repo.MarkRedeemed(ctx, campaignID, hmacs, s.now().UTC())
	if err != nil {
		return domain.RedemptionResult{}, fmt.Errorf("no se pudieron registrar los canjes: %w", err)
	}

	return domain.RedemptionResult{Matched: matched, Unknown: invalid + (len(codes) - matched)}, nil
}

// Stats devuelve los contadores agregados de una campaña.
func (s *Service) Stats(ctx context.Context, campaignID uuid.UUID) (domain.CampaignStats, error) {
	stats, err := s.repo.Stats(ctx, campaignID)
	if err != nil {
		return domain.CampaignStats{}, fmt.Errorf("no se pudieron leer las estadísticas: %w", err)
	}
	return stats, nil
}

// byteOrderMark es la marca de orden de bytes UTF-8 con la que algunas planillas
// empiezan un CSV.
const byteOrderMark = "\uFEFF"

// parseLines parte el CSV en líneas sin espacios, descartando las vacías. Devuelve
// domain.ErrTooManyLines si quedan más de domain.MaxCSVLines.
func parseLines(csv string) ([]string, error) {
	csv = strings.TrimPrefix(csv, byteOrderMark)

	raw := strings.Split(csv, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(lines) == domain.MaxCSVLines {
			return nil, domain.ErrTooManyLines
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// classify separa las líneas en códigos únicos con formato válido, líneas inválidas y
// repeticiones dentro del archivo. Conserva el orden de aparición.
func classify(lines []string) (codes []string, invalid, duplicates int) {
	seen := make(map[string]struct{}, len(lines))
	codes = make([]string, 0, len(lines))
	for _, line := range lines {
		if !domain.ValidCode(line) {
			invalid++
			continue
		}
		if _, dup := seen[line]; dup {
			duplicates++
			continue
		}
		seen[line] = struct{}{}
		codes = append(codes, line)
	}
	return codes, invalid, duplicates
}
