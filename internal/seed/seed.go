// Package seed carga datos de prueba para desarrollo: una campaña vigente con su banner
// y sus códigos, y una vencida que no debe mostrarse. Es idempotente: correrlo de nuevo
// no duplica nada.
package seed

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"time"

	"github.com/google/uuid"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/banner"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

const (
	// ActiveCodeCount es la cantidad de códigos falsos de la campaña vigente.
	ActiveCodeCount = 100
	// expiredCodeCount es la cantidad de códigos de la campaña vencida.
	expiredCodeCount = 10

	activeCodePrefix  = "PRUEBA-"
	expiredCodePrefix = "VENCIDA-"

	bannerWidth  = 640
	bannerHeight = 240
	bannerBands  = 8
)

// Los ids son fijos para que el seed sea idempotente.
var (
	// ActiveCampaignID es la campaña vigente "Estación de prueba".
	ActiveCampaignID = uuid.MustParse("00000000-0000-4000-8000-0000000000a1")
	// ExpiredCampaignID es la campaña vencida.
	ExpiredCampaignID = uuid.MustParse("00000000-0000-4000-8000-0000000000e1")
)

// codeProtector cifra los códigos y calcula su HMAC.
type codeProtector interface {
	Encrypt(code string) (string, error)
	HMAC(code string) string
}

// Run carga las dos campañas de prueba. now es el reloj de referencia.
func Run(ctx context.Context, repo domain.CampaignRepository, codes codeProtector, bannerMaxBytes int64, now time.Time) error {
	now = now.UTC()
	existing, err := repo.ListCampaigns(ctx)
	if err != nil {
		return fmt.Errorf("seed: no se pudieron listar las campañas: %w", err)
	}
	present := make(map[uuid.UUID]bool, len(existing))
	for _, c := range existing {
		present[c.ID] = true
	}

	campaigns := []struct {
		campaign domain.Campaign
		prefix   string
		count    int
		hue      color.RGBA
	}{
		{
			campaign: domain.Campaign{
				ID:          ActiveCampaignID,
				PartnerName: "Estación de prueba",
				Title:       "Estación de prueba",
				Description: "Descuento en combustible para usuarios de Vinculo Vial. Campaña de prueba con códigos falsos.",
				Terms:       "Campaña de prueba: los códigos no son válidos en ninguna estación real. Un código por persona.",
				OpenURL:     "https://example.com/estacion-de-prueba",
				StartsAt:    now.Add(-24 * time.Hour),
				EndsAt:      now.Add(30 * 24 * time.Hour),
				Active:      true,
			},
			prefix: activeCodePrefix, count: ActiveCodeCount, hue: color.RGBA{R: 0x1f, G: 0x6f, B: 0xeb, A: 0xff},
		},
		{
			campaign: domain.Campaign{
				ID:          ExpiredCampaignID,
				PartnerName: "Campaña vencida",
				Title:       "Campaña vencida",
				Description: "Campaña de prueba ya vencida: no debe aparecer en la lista de beneficios.",
				Terms:       "Campaña de prueba vencida.",
				OpenURL:     "https://example.com/campana-vencida",
				StartsAt:    now.Add(-60 * 24 * time.Hour),
				EndsAt:      now.Add(-30 * 24 * time.Hour),
				Active:      true,
			},
			prefix: expiredCodePrefix, count: expiredCodeCount, hue: color.RGBA{R: 0x8a, G: 0x8a, B: 0x8a, A: 0xff},
		},
	}

	for _, item := range campaigns {
		campaign := item.campaign
		campaign.CreatedAt, campaign.UpdatedAt = now, now
		if err := campaign.Validate(); err != nil {
			return fmt.Errorf("seed: campaña de prueba inválida: %w", err)
		}

		if !present[campaign.ID] {
			img, err := banner.Inspect(GenerateBanner(item.hue), bannerMaxBytes)
			if err != nil {
				return fmt.Errorf("seed: banner de prueba inválido: %w", err)
			}
			if err := repo.CreateCampaign(ctx, campaign, img); err != nil {
				return fmt.Errorf("seed: no se pudo crear la campaña: %w", err)
			}
		}

		batch := make([]domain.NewCode, 0, item.count)
		for i := 1; i <= item.count; i++ {
			code := fmt.Sprintf("%s%04d", item.prefix, i)
			enc, err := codes.Encrypt(code)
			if err != nil {
				return fmt.Errorf("seed: no se pudo cifrar un código: %w", err)
			}
			batch = append(batch, domain.NewCode{Enc: enc, HMAC: codes.HMAC(code)})
		}
		if _, err := repo.InsertCodes(ctx, campaign.ID, batch); err != nil {
			return fmt.Errorf("seed: no se pudieron cargar los códigos: %w", err)
		}
	}
	return nil
}

// GenerateBanner devuelve un PNG de franjas verticales del color base, para que el
// banner de prueba se distinga a simple vista sin depender de ningún archivo externo.
func GenerateBanner(base color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, bannerWidth, bannerHeight))
	bandWidth := bannerWidth / bannerBands
	for x := 0; x < bannerWidth; x++ {
		shade := uint8(255 - 20*(x/bandWidth%bannerBands))
		c := color.RGBA{R: scale(base.R, shade), G: scale(base.G, shade), B: scale(base.B, shade), A: 0xff}
		for y := 0; y < bannerHeight; y++ {
			img.SetRGBA(x, y, c)
		}
	}

	var buf bytes.Buffer
	// png.Encode solo falla si el escritor falla; bytes.Buffer nunca falla.
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// scale oscurece un canal proporcionalmente a shade (0 a 255).
func scale(channel, shade uint8) uint8 {
	return uint8(uint16(channel) * uint16(shade) / 255)
}
