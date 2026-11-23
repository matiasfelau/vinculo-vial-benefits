package postgres

import (
	"fmt"

	"gorm.io/gorm"
)

// uniqueAssignedIndexSQL garantiza que una persona tenga a lo sumo un código por
// campaña, aun con pedidos concurrentes. Es parcial: los códigos sin asignar o
// quemados (assigned_to nulo) no cuentan.
const uniqueAssignedIndexSQL = `CREATE UNIQUE INDEX IF NOT EXISTS ux_codes_campaign_assigned
	ON codes (campaign_id, assigned_to) WHERE assigned_to IS NOT NULL`

// AutoMigrate crea o actualiza el esquema de campaigns y codes.
func AutoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&campaignModel{}, &codeModel{}); err != nil {
		return fmt.Errorf("postgres: no se pudo migrar el esquema: %w", err)
	}
	if err := db.Exec(uniqueAssignedIndexSQL).Error; err != nil {
		return fmt.Errorf("postgres: no se pudo crear el índice de asignación única: %w", err)
	}
	return nil
}
