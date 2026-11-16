package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// CampaignPatch son los cambios parciales de una campaña. Un campo nil no se toca.
type CampaignPatch struct {
	Title       *string
	Description *string
	Terms       *string
	OpenURL     *string
	StartsAt    *time.Time
	EndsAt      *time.Time
	Active      *bool
}

// IsEmpty informa si el parche no cambia nada.
func (p CampaignPatch) IsEmpty() bool {
	return p.Title == nil && p.Description == nil && p.Terms == nil && p.OpenURL == nil &&
		p.StartsAt == nil && p.EndsAt == nil && p.Active == nil
}

// Apply devuelve la campaña con los cambios aplicados, sin validarla.
func (p CampaignPatch) Apply(c Campaign) Campaign {
	if p.Title != nil {
		c.Title = *p.Title
	}
	if p.Description != nil {
		c.Description = *p.Description
	}
	if p.Terms != nil {
		c.Terms = *p.Terms
	}
	if p.OpenURL != nil {
		c.OpenURL = *p.OpenURL
	}
	if p.StartsAt != nil {
		c.StartsAt = *p.StartsAt
	}
	if p.EndsAt != nil {
		c.EndsAt = *p.EndsAt
	}
	if p.Active != nil {
		c.Active = *p.Active
	}
	return c
}

// BenefitRepository es la persistencia que necesita la parte pública del servicio.
type BenefitRepository interface {
	// ListLive devuelve las campañas visibles para el usuario en now: activas, con now
	// entre starts_at y ends_at, y con códigos AVAILABLE o con un código ya asignado al
	// usuario. Van ordenadas por ends_at.
	ListLive(ctx context.Context, userID uuid.UUID, now time.Time) ([]Benefit, error)

	// BannerMeta devuelve el tipo y el ETag del banner de una campaña vigente en now, o
	// ErrBenefitNotFound.
	BannerMeta(ctx context.Context, campaignID uuid.UUID, now time.Time) (Banner, error)

	// BannerData devuelve el banner completo de una campaña vigente en now, o
	// ErrBenefitNotFound.
	BannerData(ctx context.Context, campaignID uuid.UUID, now time.Time) (Banner, error)

	// Claim asigna un código de la campaña al usuario, en una transacción, y devuelve el
	// código cifrado. Es idempotente: si el usuario ya tiene uno de esa campaña,
	// devuelve el mismo. Devuelve ErrBenefitNotFound si la campaña no existe o no está
	// vigente en now, y ErrNoCodesLeft si no queda ningún código disponible.
	Claim(ctx context.Context, campaignID, userID uuid.UUID, now time.Time) (string, error)

	// BurnByUser pasa a BURNED los códigos asignados al usuario y deja su assigned_to en
	// null (baja de cuenta). Devuelve cuántos pasó; repetirlo devuelve 0.
	BurnByUser(ctx context.Context, userID uuid.UUID) (int64, error)

	// Ping verifica la conectividad con la base de datos.
	Ping(ctx context.Context) error
}

// CampaignRepository es la persistencia que necesita la administración.
type CampaignRepository interface {
	// CreateCampaign persiste la campaña con su banner.
	CreateCampaign(ctx context.Context, campaign Campaign, banner Banner) error

	// ListCampaigns devuelve todas las campañas con sus estadísticas, de la más nueva a
	// la más vieja.
	ListCampaigns(ctx context.Context) ([]CampaignSummary, error)

	// UpdateCampaign aplica mutate sobre la campaña, bajo lock de fila, y persiste el
	// resultado si mutate no devuelve error. Devuelve ErrCampaignNotFound si no existe.
	UpdateCampaign(ctx context.Context, id uuid.UUID, now time.Time, mutate func(Campaign) (Campaign, error)) error

	// ReplaceBanner reemplaza el banner. Devuelve ErrCampaignNotFound si no existe.
	ReplaceBanner(ctx context.Context, id uuid.UUID, banner Banner, now time.Time) error

	// InsertCodes inserta los códigos en la campaña, ignorando los que ya existen, y
	// devuelve cuántos insertó. Devuelve ErrCampaignNotFound si la campaña no existe.
	InsertCodes(ctx context.Context, campaignID uuid.UUID, codes []NewCode) (int, error)

	// MarkRedeemed marca redeemed_at en los códigos de la campaña cuyo HMAC está en
	// hmacs, y devuelve cuántos de esos HMAC corresponden a un código de la campaña
	// (aunque ya estuviera canjeado). Devuelve ErrCampaignNotFound si no existe.
	MarkRedeemed(ctx context.Context, campaignID uuid.UUID, hmacs []string, now time.Time) (int, error)

	// Stats devuelve los contadores de la campaña, o ErrCampaignNotFound.
	Stats(ctx context.Context, campaignID uuid.UUID) (CampaignStats, error)
}
