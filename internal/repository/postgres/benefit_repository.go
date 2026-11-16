package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

// liveCondition es la condición de campaña vigente sobre la tabla campaigns: activa y
// con now entre starts_at (inclusive) y ends_at (exclusive). Lleva dos parámetros, el
// mismo now las dos veces.
const liveCondition = `active AND starts_at <= ? AND ends_at > ?`

// benefitRow es una fila del listado público: la campaña y, si el usuario ya lo pidió,
// su código cifrado.
type benefitRow struct {
	ID          uuid.UUID      `gorm:"column:id"`
	PartnerName string         `gorm:"column:partner_name"`
	Title       string         `gorm:"column:title"`
	Description string         `gorm:"column:description"`
	Terms       string         `gorm:"column:terms"`
	OpenURL     string         `gorm:"column:open_url"`
	EndsAt      time.Time      `gorm:"column:ends_at"`
	CodeEnc     sql.NullString `gorm:"column:code_enc"`
}

// ListLive implementa domain.BenefitRepository. Una campaña se lista si tiene códigos
// AVAILABLE o si el usuario ya tiene uno asignado en ella.
func (r *Repository) ListLive(ctx context.Context, userID uuid.UUID, now time.Time) ([]domain.Benefit, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	const query = `
SELECT c.id, c.partner_name, c.title, c.description, c.terms, c.open_url, c.ends_at, mine.code_enc
FROM campaigns c
LEFT JOIN codes mine
       ON mine.campaign_id = c.id AND mine.assigned_to = ? AND mine.status = ?
WHERE c.active AND c.starts_at <= ? AND c.ends_at > ?
  AND (mine.id IS NOT NULL
       OR EXISTS (SELECT 1 FROM codes pool WHERE pool.campaign_id = c.id AND pool.status = ?))
ORDER BY c.ends_at ASC, c.id ASC`

	var rows []benefitRow
	if err := r.db.WithContext(ctx).Raw(query, userID, statusAssigned, now, now, statusAvailable).Scan(&rows).Error; err != nil {
		return nil, classifyError(err)
	}

	benefits := make([]domain.Benefit, 0, len(rows))
	for _, row := range rows {
		benefits = append(benefits, domain.Benefit{
			ID:          row.ID,
			PartnerName: row.PartnerName,
			Title:       row.Title,
			Description: row.Description,
			Terms:       row.Terms,
			OpenURL:     row.OpenURL,
			EndsAt:      row.EndsAt,
			CodeEnc:     row.CodeEnc.String,
		})
	}
	return benefits, nil
}

// bannerRow es el banner de una campaña. Data queda vacío cuando solo se pide el tipo y
// el ETag.
type bannerRow struct {
	ContentType string `gorm:"column:banner_content_type"`
	ETag        string `gorm:"column:banner_etag"`
	Data        []byte `gorm:"column:banner"`
}

// BannerMeta implementa domain.BenefitRepository.
func (r *Repository) BannerMeta(ctx context.Context, campaignID uuid.UUID, now time.Time) (domain.Banner, error) {
	return r.findBanner(ctx, `banner_content_type, banner_etag`, campaignID, now)
}

// BannerData implementa domain.BenefitRepository.
func (r *Repository) BannerData(ctx context.Context, campaignID uuid.UUID, now time.Time) (domain.Banner, error) {
	return r.findBanner(ctx, `banner_content_type, banner_etag, banner`, campaignID, now)
}

// findBanner lee las columnas del banner de una campaña vigente. columns es una
// constante del paquete, nunca entrada externa.
func (r *Repository) findBanner(ctx context.Context, columns string, campaignID uuid.UUID, now time.Time) (domain.Banner, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	var rows []bannerRow
	err := r.db.WithContext(ctx).
		Raw(`SELECT `+columns+` FROM campaigns WHERE id = ? AND `+liveCondition, campaignID, now, now).
		Scan(&rows).Error
	if err != nil {
		return domain.Banner{}, classifyError(err)
	}
	if len(rows) == 0 {
		return domain.Banner{}, domain.ErrBenefitNotFound
	}
	return domain.Banner{ContentType: rows[0].ContentType, ETag: rows[0].ETag, Data: rows[0].Data}, nil
}

// claimedCode es el código disponible que una transacción de claim toma para sí.
type claimedCode struct {
	ID      uuid.UUID `gorm:"column:id"`
	CodeEnc string    `gorm:"column:code_enc"`
}

// Claim implementa domain.BenefitRepository.
//
// Todo ocurre en una transacción. El lock de aviso por (campaña, usuario) serializa los
// pedidos simultáneos de una misma persona: el segundo espera, ve el código del primero
// y lo devuelve. Dos personas distintas no se bloquean entre sí: cada una toma un código
// distinto con FOR UPDATE SKIP LOCKED, que salta los que otra transacción ya reservó. El
// índice único parcial sobre (campaign_id, assigned_to) es la última defensa.
func (r *Repository) Claim(ctx context.Context, campaignID, userID uuid.UUID, now time.Time) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	var codeEnc string
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var live int64
		if err := tx.Raw(`SELECT count(*) FROM campaigns WHERE id = ? AND `+liveCondition, campaignID, now, now).Scan(&live).Error; err != nil {
			return err
		}
		if live == 0 {
			return &domainError{err: domain.ErrBenefitNotFound}
		}

		if err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`, campaignID.String()+":"+userID.String()).Error; err != nil {
			return err
		}

		var mine []string
		if err := tx.Raw(`SELECT code_enc FROM codes WHERE campaign_id = ? AND assigned_to = ? AND status = ?`,
			campaignID, userID, statusAssigned).Scan(&mine).Error; err != nil {
			return err
		}
		if len(mine) > 0 {
			codeEnc = mine[0]
			return nil
		}

		var free []claimedCode
		if err := tx.Raw(`SELECT id, code_enc FROM codes WHERE campaign_id = ? AND status = ? LIMIT 1 FOR UPDATE SKIP LOCKED`,
			campaignID, statusAvailable).Scan(&free).Error; err != nil {
			return err
		}
		if len(free) == 0 {
			return &domainError{err: domain.ErrNoCodesLeft}
		}

		if err := tx.Exec(`UPDATE codes SET status = ?, assigned_to = ?, assigned_at = ? WHERE id = ?`,
			statusAssigned, userID, now, free[0].ID).Error; err != nil {
			return err
		}
		codeEnc = free[0].CodeEnc
		return nil
	})
	if err != nil {
		return "", classifyError(err)
	}
	return codeEnc, nil
}

// BurnByUser implementa domain.BenefitRepository. Es un único UPDATE: repetirlo no
// encuentra filas con assigned_to del usuario, así que es idempotente.
func (r *Repository) BurnByUser(ctx context.Context, userID uuid.UUID) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	var burned int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Exec(`UPDATE codes SET status = ?, assigned_to = NULL WHERE assigned_to = ? AND status = ?`,
			statusBurned, userID, statusAssigned)
		burned = result.RowsAffected
		return result.Error
	})
	if err != nil {
		return 0, classifyError(err)
	}
	return burned, nil
}
