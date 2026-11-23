package domain

import (
	"time"

	"github.com/google/uuid"
)

// CodeStatus es el estado de un código de descuento.
type CodeStatus string

const (
	// CodeAvailable es un código sin asignar, en el pool de la campaña.
	CodeAvailable CodeStatus = "AVAILABLE"
	// CodeAssigned es un código entregado a una persona.
	CodeAssigned CodeStatus = "ASSIGNED"
	// CodeBurned es un código que no vuelve al pool: su dueño dio de baja la cuenta y pudo
	// haberlo visto o canjeado.
	CodeBurned CodeStatus = "BURNED"
)

// Límites de un código y de una carga.
const (
	// MinCodeLength y MaxCodeLength acotan el largo de un código.
	MinCodeLength = 4
	MaxCodeLength = 64
	// MaxCSVLines es el máximo de líneas de una carga de códigos o de canjes.
	MaxCSVLines = 100000
)

// ValidCode informa si code cumple el formato de un código: de 4 a 64 caracteres
// [A-Za-z0-9-].
func ValidCode(code string) bool {
	if len(code) < MinCodeLength || len(code) > MaxCodeLength {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// NewCode es un código listo para persistir: cifrado y con su HMAC.
type NewCode struct {
	Enc  string
	HMAC string
}

// CampaignStats son los contadores agregados de una campaña. Es lo único que se
// comparte con el partner: nunca datos de los usuarios.
type CampaignStats struct {
	Available int64
	Assigned  int64
	Burned    int64
	Redeemed  int64
}

// UploadResult es el resultado de una carga de códigos.
type UploadResult struct {
	Inserted   int
	Duplicates int
	Invalid    int
}

// RedemptionResult es el resultado de un reporte de canjes.
type RedemptionResult struct {
	Matched int
	Unknown int
}

// Benefit es una campaña tal como la ve un usuario: con su código si ya lo pidió.
type Benefit struct {
	ID          uuid.UUID
	PartnerName string
	Title       string
	Description string
	Terms       string
	OpenURL     string
	EndsAt      time.Time
	// CodeEnc es el código asignado al usuario, cifrado; vacío si todavía no lo pidió.
	CodeEnc string
}

// Banner es la imagen de una campaña.
type Banner struct {
	ContentType string
	ETag        string
	Data        []byte
}

// CampaignSummary es una campaña con sus estadísticas, para el listado de administración.
type CampaignSummary struct {
	Campaign
	BannerETag string
	Stats      CampaignStats
}
