package postgres

import (
	"time"

	"github.com/google/uuid"
)

// campaignModel es la fila de campaigns. El banner vive en la propia fila (bytea): lo
// sirve este servicio, sin un bucket aparte.
type campaignModel struct {
	ID                uuid.UUID `gorm:"type:uuid;primaryKey"`
	PartnerName       string    `gorm:"type:varchar(80);not null"`
	Title             string    `gorm:"type:varchar(80);not null"`
	Description       string    `gorm:"type:varchar(500);not null"`
	Terms             string    `gorm:"type:varchar(2000);not null"`
	OpenURL           string    `gorm:"column:open_url;type:varchar(500);not null"`
	StartsAt          time.Time `gorm:"type:timestamptz;not null"`
	EndsAt            time.Time `gorm:"type:timestamptz;not null;check:ends_at > starts_at"`
	Active            bool      `gorm:"not null;default:false"`
	Banner            []byte    `gorm:"type:bytea;not null"`
	BannerContentType string    `gorm:"type:varchar(32);not null"`
	BannerETag        string    `gorm:"column:banner_etag;type:varchar(64);not null"`
	CreatedAt         time.Time `gorm:"type:timestamptz;not null"`
	UpdatedAt         time.Time `gorm:"type:timestamptz;not null"`
}

// TableName fija el nombre de la tabla.
func (campaignModel) TableName() string { return "campaigns" }

// codeModel es la fila de codes. code_enc es AES-256-GCM; code_hmac, el HMAC-SHA256
// del código, único por campaña. assigned_to es único por campaña mientras no es nulo
// (índice parcial creado en AutoMigrate).
type codeModel struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey"`
	CampaignID uuid.UUID  `gorm:"type:uuid;not null;uniqueIndex:ux_codes_campaign_hmac,priority:1;index:ix_codes_campaign_status,priority:1"`
	CodeEnc    string     `gorm:"type:text;not null"`
	CodeHMAC   string     `gorm:"column:code_hmac;type:varchar(64);not null;uniqueIndex:ux_codes_campaign_hmac,priority:2"`
	Status     string     `gorm:"type:varchar(16);not null;index:ix_codes_campaign_status,priority:2;check:status IN ('AVAILABLE','ASSIGNED','BURNED')"`
	AssignedTo *uuid.UUID `gorm:"type:uuid"`
	AssignedAt *time.Time `gorm:"type:timestamptz"`
	RedeemedAt *time.Time `gorm:"type:timestamptz"`

	// Campaign declara la clave foránea codes.campaign_id → campaigns.id. Nunca se carga.
	Campaign *campaignModel `gorm:"foreignKey:CampaignID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`
}

// TableName fija el nombre de la tabla.
func (codeModel) TableName() string { return "codes" }
