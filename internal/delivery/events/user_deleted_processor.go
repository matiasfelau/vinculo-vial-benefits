package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// eventTypeUserDeleted es el eventType (y routing key) con el que Identity informa la
// baja de una cuenta.
const eventTypeUserDeleted = "user.deleted"

// userCodesBurner quema los códigos de un usuario dado de baja y devuelve cuántos quemó.
type userCodesBurner interface {
	Execute(ctx context.Context, userID uuid.UUID) (int64, error)
}

// UserDeletedProcessor es el events.Processor de user.deleted: valida el envelope y el
// payload contra el contrato y delega en el caso de uso el paso a BURNED de los códigos
// del usuario. Todo mensaje que no cumple el contrato es inválido (va a la DLQ sin
// reintento); cualquier otro error del caso de uso es transitorio.
type UserDeletedProcessor struct {
	burner userCodesBurner
}

var _ Processor = (*UserDeletedProcessor)(nil)

// NewUserDeletedProcessor construye el procesador sobre el caso de uso.
func NewUserDeletedProcessor(burner userCodesBurner) *UserDeletedProcessor {
	return &UserDeletedProcessor{burner: burner}
}

// Process implementa Processor. Es idempotente porque lo es el caso de uso: repetir la
// entrega de un mismo user.deleted no encuentra códigos asignados que quemar. Loguea el
// eventId y la cantidad, nunca el usuario ni los códigos.
func (p *UserDeletedProcessor) Process(ctx context.Context, envelope InboundEnvelope) error {
	userID, err := parseUserDeleted(envelope)
	if err != nil {
		return err
	}

	burned, err := p.burner.Execute(ctx, userID)
	if err != nil {
		return fmt.Errorf("user.deleted: %w", err)
	}

	slog.Info("consumidor: códigos del usuario dado de baja quemados", "event_id", envelope.EventID, "codigos_quemados", burned)
	return nil
}

// userDeletedPayload es el payload de user.deleted. Benefits no usa deleted_at, pero el
// contrato lo exige y un payload sin él no es un user.deleted válido.
type userDeletedPayload struct {
	DeletedAt *string `json:"deleted_at"`
}

// parseUserDeleted valida el envelope y el payload de user.deleted y devuelve el usuario
// dado de baja, que es el actor del evento. Todo error que devuelve envuelve
// ErrInvalidMessage y ninguno refleja el contenido del mensaje.
func parseUserDeleted(envelope InboundEnvelope) (uuid.UUID, error) {
	if envelope.EventType != eventTypeUserDeleted {
		return uuid.Nil, invalid("eventType inesperado")
	}

	if envelope.Actor == nil {
		return uuid.Nil, invalid("actor ausente")
	}
	userID, err := uuid.Parse(*envelope.Actor)
	if err != nil || userID == uuid.Nil {
		return uuid.Nil, invalid("actor inválido")
	}

	var payload userDeletedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return uuid.Nil, invalid("payload ilegible")
	}
	if payload.DeletedAt == nil {
		return uuid.Nil, invalid("deleted_at ausente")
	}
	if _, err := time.Parse(time.RFC3339, *payload.DeletedAt); err != nil {
		return uuid.Nil, invalid("deleted_at no es RFC3339")
	}

	return userID, nil
}
