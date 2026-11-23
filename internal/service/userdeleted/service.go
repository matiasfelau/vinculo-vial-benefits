// Package userdeleted implementa la reacción de Benefits a la baja de una cuenta.
package userdeleted

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// codeBurner es la persistencia que necesita el caso de uso.
type codeBurner interface {
	// BurnByUser pasa a BURNED los códigos del usuario y devuelve cuántos pasó.
	BurnByUser(ctx context.Context, userID uuid.UUID) (int64, error)
}

// BurnCodesService quema los códigos de un usuario dado de baja.
type BurnCodesService struct {
	codes codeBurner
}

// NewBurnCodesService construye el caso de uso sobre la persistencia.
func NewBurnCodesService(codes codeBurner) *BurnCodesService {
	return &BurnCodesService{codes: codes}
}

// Execute pasa a BURNED los códigos del usuario, deja su assigned_to en null y devuelve
// cuántos pasó. No vuelven al pool: el usuario pudo haberlos visto o canjeado. Es
// idempotente: si ya no tiene ninguno asignado (porque el evento se entregó de nuevo, o
// nunca pidió uno) devuelve 0 sin error.
func (s *BurnCodesService) Execute(ctx context.Context, userID uuid.UUID) (int64, error) {
	burned, err := s.codes.BurnByUser(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("no se pudieron quemar los códigos del usuario: %w", err)
	}
	return burned, nil
}
