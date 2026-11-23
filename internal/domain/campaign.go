package domain

import (
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Límites de los campos de una campaña, iguales a los de las columnas.
const (
	MaxPartnerNameLength  = 80
	MaxTitleLength        = 80
	MaxDescriptionLength  = 500
	MaxTermsLength        = 2000
	MaxOpenURLLength      = 500
	openURLRequiredScheme = "https"
)

// Campaign es una campaña de un partner: un lote de códigos de descuento que
// Vinculo Vial distribuye, un código por persona.
type Campaign struct {
	ID          uuid.UUID
	PartnerName string
	Title       string
	Description string
	Terms       string
	// OpenURL es el App Link del partner o su ficha de Play Store. Solo https.
	OpenURL  string
	StartsAt time.Time
	EndsAt   time.Time
	Active   bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsLive informa si la campaña está vigente en now: activa y con now dentro de
// [StartsAt, EndsAt).
func (c Campaign) IsLive(now time.Time) bool {
	return c.Active && !now.Before(c.StartsAt) && now.Before(c.EndsAt)
}

// Validate comprueba que la campaña cumpla el contrato. El error envuelve
// ErrInvalidCampaign y nombra el campo, nunca su valor.
func (c Campaign) Validate() error {
	checks := []struct {
		field string
		value string
		max   int
	}{
		{"partner_name", c.PartnerName, MaxPartnerNameLength},
		{"title", c.Title, MaxTitleLength},
		{"description", c.Description, MaxDescriptionLength},
		{"terms", c.Terms, MaxTermsLength},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return invalidField(check.field, "es obligatorio")
		}
		if utf8.RuneCountInString(check.value) > check.max {
			return invalidField(check.field, fmt.Sprintf("supera los %d caracteres", check.max))
		}
		if !utf8.ValidString(check.value) {
			return invalidField(check.field, "no es UTF-8 válido")
		}
	}

	if err := ValidateOpenURL(c.OpenURL); err != nil {
		return err
	}

	if c.StartsAt.IsZero() {
		return invalidField("starts_at", "es obligatorio")
	}
	if c.EndsAt.IsZero() {
		return invalidField("ends_at", "es obligatorio")
	}
	if !c.EndsAt.After(c.StartsAt) {
		return invalidField("ends_at", "debe ser posterior a starts_at")
	}
	return nil
}

// ValidateOpenURL comprueba que raw sea una URL https absoluta, con host y sin
// credenciales embebidas. El destino lo abre la app del usuario; este servicio nunca
// la consulta.
func ValidateOpenURL(raw string) error {
	if raw == "" {
		return invalidField("open_url", "es obligatorio")
	}
	if utf8.RuneCountInString(raw) > MaxOpenURLLength {
		return invalidField("open_url", fmt.Sprintf("supera los %d caracteres", MaxOpenURLLength))
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != openURLRequiredScheme || parsed.Hostname() == "" || parsed.User != nil {
		return invalidField("open_url", "debe ser una URL https absoluta sin credenciales")
	}
	return nil
}

func invalidField(field, reason string) error {
	return fmt.Errorf("%w: %s %s", ErrInvalidCampaign, field, reason)
}
