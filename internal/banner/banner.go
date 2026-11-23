// Package banner valida y describe las imágenes de las campañas.
package banner

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

// allowedTypes son los formatos admitidos. El tipo se detecta por los bytes, nunca por
// la extensión ni por el Content-Type declarado por el cliente.
var allowedTypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/webp": {},
}

// Inspect valida data y devuelve el banner con su tipo detectado por firma de bytes y
// su ETag (SHA-256 en hexadecimal). Devuelve ErrBannerTooLarge si supera maxBytes y
// ErrInvalidBanner si no es PNG, JPEG ni WebP.
func Inspect(data []byte, maxBytes int64) (domain.Banner, error) {
	if int64(len(data)) > maxBytes {
		return domain.Banner{}, domain.ErrBannerTooLarge
	}
	if len(data) == 0 {
		return domain.Banner{}, domain.ErrInvalidBanner
	}

	contentType := http.DetectContentType(data)
	if _, ok := allowedTypes[contentType]; !ok {
		return domain.Banner{}, domain.ErrInvalidBanner
	}

	sum := sha256.Sum256(data)
	return domain.Banner{ContentType: contentType, ETag: hex.EncodeToString(sum[:]), Data: data}, nil
}
