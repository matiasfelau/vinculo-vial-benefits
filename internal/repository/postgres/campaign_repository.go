package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

// campaignColumns son las columnas de una campaña sin el banner: la imagen puede pesar
// cientos de KiB y casi ningún caso de uso la necesita.
const campaignColumns = `id, partner_name, title, description, terms, open_url, starts_at, ends_at, active, banner_etag, created_at, updated_at`

// summaryRow es una campaña con sus contadores, para el listado de administración.
type summaryRow struct {
	ID          uuid.UUID `gorm:"column:id"`
	PartnerName string    `gorm:"column:partner_name"`
	Title       string    `gorm:"column:title"`
	Description string    `gorm:"column:description"`
	Terms       string    `gorm:"column:terms"`
	OpenURL     string    `gorm:"column:open_url"`
	StartsAt    time.Time `gorm:"column:starts_at"`
	EndsAt      time.Time `gorm:"column:ends_at"`
	Active      bool      `gorm:"column:active"`
	BannerETag  string    `gorm:"column:banner_etag"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
	Available   int64     `gorm:"column:available"`
	Assigned    int64     `gorm:"column:assigned"`
	Burned      int64     `gorm:"column:burned"`
	Redeemed    int64     `gorm:"column:redeemed"`
}

func (s summaryRow) toDomain() domain.CampaignSummary {
	return domain.CampaignSummary{
		Campaign: domain.Campaign{
			ID: s.ID, PartnerName: s.PartnerName, Title: s.Title, Description: s.Description,
			Terms: s.Terms, OpenURL: s.OpenURL, StartsAt: s.StartsAt, EndsAt: s.EndsAt,
			Active: s.Active, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
		},
		BannerETag: s.BannerETag,
		Stats:      domain.CampaignStats{Available: s.Available, Assigned: s.Assigned, Burned: s.Burned, Redeemed: s.Redeemed},
	}
}

// CreateCampaign implementa domain.CampaignRepository.
func (r *Repository) CreateCampaign(ctx context.Context, campaign domain.Campaign, banner domain.Banner) error {
	ctx, cancel := context.WithTimeout(ctx, r.adminTimeout)
	defer cancel()

	model := campaignModel{
		ID:                campaign.ID,
		PartnerName:       campaign.PartnerName,
		Title:             campaign.Title,
		Description:       campaign.Description,
		Terms:             campaign.Terms,
		OpenURL:           campaign.OpenURL,
		StartsAt:          campaign.StartsAt,
		EndsAt:            campaign.EndsAt,
		Active:            campaign.Active,
		Banner:            banner.Data,
		BannerContentType: banner.ContentType,
		BannerETag:        banner.ETag,
		CreatedAt:         campaign.CreatedAt,
		UpdatedAt:         campaign.UpdatedAt,
	}
	return classifyError(r.db.WithContext(ctx).Create(&model).Error)
}

// ListCampaigns implementa domain.CampaignRepository.
func (r *Repository) ListCampaigns(ctx context.Context) ([]domain.CampaignSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, r.adminTimeout)
	defer cancel()

	const query = `
SELECT c.id, c.partner_name, c.title, c.description, c.terms, c.open_url, c.starts_at, c.ends_at,
       c.active, c.banner_etag, c.created_at, c.updated_at,
       COUNT(k.id) FILTER (WHERE k.status = ?) AS available,
       COUNT(k.id) FILTER (WHERE k.status = ?) AS assigned,
       COUNT(k.id) FILTER (WHERE k.status = ?) AS burned,
       COUNT(k.id) FILTER (WHERE k.redeemed_at IS NOT NULL) AS redeemed
FROM campaigns c
LEFT JOIN codes k ON k.campaign_id = c.id
GROUP BY c.id
ORDER BY c.created_at DESC, c.id ASC`

	var rows []summaryRow
	if err := r.db.WithContext(ctx).Raw(query, statusAvailable, statusAssigned, statusBurned).Scan(&rows).Error; err != nil {
		return nil, classifyError(err)
	}

	summaries := make([]domain.CampaignSummary, 0, len(rows))
	for _, row := range rows {
		summaries = append(summaries, row.toDomain())
	}
	return summaries, nil
}

// UpdateCampaign implementa domain.CampaignRepository. La fila se bloquea con FOR UPDATE
// mientras mutate la valida, para que dos parches simultáneos no se pisen.
func (r *Repository) UpdateCampaign(ctx context.Context, id uuid.UUID, now time.Time, mutate func(domain.Campaign) (domain.Campaign, error)) error {
	ctx, cancel := context.WithTimeout(ctx, r.adminTimeout)
	defer cancel()

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []summaryRow
		if err := tx.Raw(`SELECT `+campaignColumns+` FROM campaigns WHERE id = ? FOR UPDATE`, id).Scan(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return &domainError{err: domain.ErrCampaignNotFound}
		}

		updated, err := mutate(rows[0].toDomain().Campaign)
		if err != nil {
			return &domainError{err: err}
		}

		return tx.Exec(`UPDATE campaigns SET title = ?, description = ?, terms = ?, open_url = ?, starts_at = ?, ends_at = ?, active = ?, updated_at = ? WHERE id = ?`,
			updated.Title, updated.Description, updated.Terms, updated.OpenURL, updated.StartsAt, updated.EndsAt, updated.Active, now, id).Error
	})
	return classifyError(err)
}

// ReplaceBanner implementa domain.CampaignRepository.
func (r *Repository) ReplaceBanner(ctx context.Context, id uuid.UUID, banner domain.Banner, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, r.adminTimeout)
	defer cancel()

	result := r.db.WithContext(ctx).Exec(
		`UPDATE campaigns SET banner = ?, banner_content_type = ?, banner_etag = ?, updated_at = ? WHERE id = ?`,
		banner.Data, banner.ContentType, banner.ETag, now, id)
	if result.Error != nil {
		return classifyError(result.Error)
	}
	if result.RowsAffected == 0 {
		return domain.ErrCampaignNotFound
	}
	return nil
}

// requireCampaign devuelve ErrCampaignNotFound si la campaña no existe.
func requireCampaign(tx *gorm.DB, id uuid.UUID) error {
	var found int64
	if err := tx.Raw(`SELECT count(*) FROM campaigns WHERE id = ?`, id).Scan(&found).Error; err != nil {
		return err
	}
	if found == 0 {
		return &domainError{err: domain.ErrCampaignNotFound}
	}
	return nil
}

// InsertCodes implementa domain.CampaignRepository. La carga es todo o nada: una sola
// transacción, en lotes. Los códigos que ya existen en la campaña se ignoran.
func (r *Repository) InsertCodes(ctx context.Context, campaignID uuid.UUID, codes []domain.NewCode) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, r.adminTimeout)
	defer cancel()

	inserted := 0
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireCampaign(tx, campaignID); err != nil {
			return err
		}

		for start := 0; start < len(codes); start += batchSize {
			end := min(start+batchSize, len(codes))
			batch := make([]codeModel, 0, end-start)
			for _, code := range codes[start:end] {
				batch = append(batch, codeModel{
					ID:         uuid.New(),
					CampaignID: campaignID,
					CodeEnc:    code.Enc,
					CodeHMAC:   code.HMAC,
					Status:     statusAvailable,
				})
			}

			result := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "campaign_id"}, {Name: "code_hmac"}},
				DoNothing: true,
			}).Create(&batch)
			if result.Error != nil {
				return result.Error
			}
			inserted += int(result.RowsAffected)
		}
		return nil
	})
	if err != nil {
		return 0, classifyError(err)
	}
	return inserted, nil
}

// MarkRedeemed implementa domain.CampaignRepository. redeemed_at conserva la primera
// fecha: reportar dos veces el mismo canje no la mueve.
func (r *Repository) MarkRedeemed(ctx context.Context, campaignID uuid.UUID, hmacs []string, now time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, r.adminTimeout)
	defer cancel()

	matched := 0
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireCampaign(tx, campaignID); err != nil {
			return err
		}

		for start := 0; start < len(hmacs); start += batchSize {
			end := min(start+batchSize, len(hmacs))
			result := tx.Exec(`UPDATE codes SET redeemed_at = COALESCE(redeemed_at, ?) WHERE campaign_id = ? AND code_hmac IN ?`,
				now, campaignID, hmacs[start:end])
			if result.Error != nil {
				return result.Error
			}
			matched += int(result.RowsAffected)
		}
		return nil
	})
	if err != nil {
		return 0, classifyError(err)
	}
	return matched, nil
}

// statsRow son los contadores de una campaña.
type statsRow struct {
	Available int64 `gorm:"column:available"`
	Assigned  int64 `gorm:"column:assigned"`
	Burned    int64 `gorm:"column:burned"`
	Redeemed  int64 `gorm:"column:redeemed"`
}

// Stats implementa domain.CampaignRepository.
func (r *Repository) Stats(ctx context.Context, campaignID uuid.UUID) (domain.CampaignStats, error) {
	ctx, cancel := context.WithTimeout(ctx, r.adminTimeout)
	defer cancel()

	var stats domain.CampaignStats
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireCampaign(tx, campaignID); err != nil {
			return err
		}

		var rows []statsRow
		if err := tx.Raw(`
SELECT COUNT(*) FILTER (WHERE status = ?) AS available,
       COUNT(*) FILTER (WHERE status = ?) AS assigned,
       COUNT(*) FILTER (WHERE status = ?) AS burned,
       COUNT(*) FILTER (WHERE redeemed_at IS NOT NULL) AS redeemed
FROM codes WHERE campaign_id = ?`, statusAvailable, statusAssigned, statusBurned, campaignID).Scan(&rows).Error; err != nil {
			return err
		}
		stats = domain.CampaignStats{Available: rows[0].Available, Assigned: rows[0].Assigned, Burned: rows[0].Burned, Redeemed: rows[0].Redeemed}
		return nil
	})
	if err != nil {
		return domain.CampaignStats{}, classifyError(err)
	}
	return stats, nil
}
