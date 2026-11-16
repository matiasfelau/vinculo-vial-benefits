package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// quemadorRegistrado simula el caso de uso sobre un conjunto de códigos por
// usuario: quema los del usuario y retorna cuántos quemó, como lo hace la base.
type quemadorRegistrado struct {
	err      error
	codigos  map[uuid.UUID]int64
	llamadas []uuid.UUID
}

func nuevoQuemador(codigosPorUsuario map[uuid.UUID]int64) *quemadorRegistrado {
	return &quemadorRegistrado{codigos: codigosPorUsuario}
}

func (r *quemadorRegistrado) Execute(_ context.Context, userID uuid.UUID) (int64, error) {
	r.llamadas = append(r.llamadas, userID)
	if r.err != nil {
		return 0, r.err
	}
	borrados := r.codigos[userID]
	delete(r.codigos, userID)
	return borrados, nil
}

func actorDe(id uuid.UUID) *string {
	s := id.String()
	return &s
}

func payloadDeBaja() map[string]any {
	return map[string]any{"deleted_at": "2026-11-01T12:00:00Z"}
}

func sobreDeBaja(t *testing.T, actor *string, payload any) InboundEnvelope {
	t.Helper()

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("no se pudo serializar el payload: %v", err)
	}
	return InboundEnvelope{
		EventID:   uuid.New(),
		EventType: "user.deleted",
		Timestamp: time.Now().UTC(),
		Producer:  "vinculo-vial-identity",
		Actor:     actor,
		Payload:   raw,
	}
}

// cuerpoDeBaja serializa el envelope como lo publicaría Identity.
func cuerpoDeBaja(t *testing.T, envelope InboundEnvelope) []byte {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"eventId": envelope.EventID, "eventType": envelope.EventType, "timestamp": envelope.Timestamp,
		"producer": envelope.Producer, "actor": envelope.Actor, "payload": envelope.Payload,
	})
	if err != nil {
		t.Fatalf("no se pudo serializar el envelope: %v", err)
	}
	return body
}

func TestParseUserDeletedDevuelveElActorDelEvento(t *testing.T) {
	for nombre, deletedAt := range map[string]string{
		"UTC":                  "2026-11-01T12:00:00Z",
		"con zona horaria":     "2026-11-01T09:00:00-03:00",
		"con fracción de seg.": "2026-11-01T12:00:00.123456Z",
	} {
		t.Run(nombre, func(t *testing.T) {
			userID := uuid.New()

			got, err := parseUserDeleted(sobreDeBaja(t, actorDe(userID), map[string]any{"deleted_at": deletedAt}))
			if err != nil {
				t.Fatalf("error inesperado: %v", err)
			}
			if got != userID {
				t.Errorf("usuario = %v, esperaba %v", got, userID)
			}
		})
	}
}

func TestParseUserDeletedMensajesInvalidosEnvuelvenErrInvalidMessage(t *testing.T) {
	otroEvento := sobreDeBaja(t, actorDe(uuid.New()), payloadDeBaja())
	otroEvento.EventType = "user.sessions.revoked"
	nilUUID := uuid.Nil.String()
	noEsUUID := "abc"
	vacio := ""

	casos := map[string]InboundEnvelope{
		"eventType de otro evento":  otroEvento,
		"actor ausente":             sobreDeBaja(t, nil, payloadDeBaja()),
		"actor no es UUID":          sobreDeBaja(t, &noEsUUID, payloadDeBaja()),
		"actor vacío":               sobreDeBaja(t, &vacio, payloadDeBaja()),
		"actor es el UUID nulo":     sobreDeBaja(t, &nilUUID, payloadDeBaja()),
		"payload ausente de campos": sobreDeBaja(t, actorDe(uuid.New()), map[string]any{}),
		"deleted_at null":           sobreDeBaja(t, actorDe(uuid.New()), map[string]any{"deleted_at": nil}),
		"deleted_at no es fecha":    sobreDeBaja(t, actorDe(uuid.New()), map[string]any{"deleted_at": "ayer"}),
		"deleted_at sin zona":       sobreDeBaja(t, actorDe(uuid.New()), map[string]any{"deleted_at": "2026-11-01T12:00:00"}),
		"deleted_at es un número":   sobreDeBaja(t, actorDe(uuid.New()), map[string]any{"deleted_at": 1793534400}),
		"payload que no es objeto":  {EventID: uuid.New(), EventType: "user.deleted", Actor: actorDe(uuid.New()), Payload: json.RawMessage(`[1]`)},
		"payload ilegible":          {EventID: uuid.New(), EventType: "user.deleted", Actor: actorDe(uuid.New()), Payload: json.RawMessage(`{no es json`)},
	}

	for nombre, envelope := range casos {
		t.Run(nombre, func(t *testing.T) {
			if _, err := parseUserDeleted(envelope); !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("error = %v, esperaba que envolviera ErrInvalidMessage", err)
			}
		})
	}
}

func TestParseUserDeletedElErrorNoReflejaElContenidoDelMensaje(t *testing.T) {
	secreto := "SECRETO-ACTOR"

	_, err := parseUserDeleted(sobreDeBaja(t, &secreto, map[string]any{"deleted_at": "SECRETO-FECHA"}))

	if err == nil || strings.Contains(err.Error(), "SECRETO") {
		t.Fatalf("el error refleja el contenido del mensaje: %v", err)
	}
}

func TestUserDeletedProcessorQuemaLosCodigosDelActor(t *testing.T) {
	userID, otro := uuid.New(), uuid.New()
	quemador := nuevoQuemador(map[uuid.UUID]int64{userID: 3, otro: 2})

	err := NewUserDeletedProcessor(quemador).Process(context.Background(), sobreDeBaja(t, actorDe(userID), payloadDeBaja()))

	if err != nil {
		t.Fatalf("error inesperado: %v", err)
	}
	if len(quemador.llamadas) != 1 || quemador.llamadas[0] != userID {
		t.Fatalf("llamadas = %v, esperaba una sola, para %v", quemador.llamadas, userID)
	}
	if _, quedan := quemador.codigos[userID]; quedan {
		t.Error("los códigos del actor debían quemarse")
	}
	if quemador.codigos[otro] != 2 {
		t.Error("los códigos de otro usuario no debían tocarse")
	}
}

func TestUserDeletedProcessorMensajeInvalidoNoLlegaAlCasoDeUso(t *testing.T) {
	quemador := nuevoQuemador(nil)

	err := NewUserDeletedProcessor(quemador).Process(context.Background(), sobreDeBaja(t, nil, payloadDeBaja()))

	if !errors.Is(err, ErrInvalidMessage) || len(quemador.llamadas) != 0 {
		t.Fatalf("error = %v, llamadas = %d; esperaba ErrInvalidMessage sin ejecutar nada", err, len(quemador.llamadas))
	}
}

func TestUserDeletedProcessorElFalloDelCasoDeUsoEsTransitorio(t *testing.T) {
	causa := errors.New("base de datos caída")
	quemador := nuevoQuemador(nil)
	quemador.err = causa

	err := NewUserDeletedProcessor(quemador).Process(context.Background(), sobreDeBaja(t, actorDe(uuid.New()), payloadDeBaja()))

	if !errors.Is(err, causa) {
		t.Fatalf("error = %v, esperaba que conservara la causa", err)
	}
	if errors.Is(err, ErrInvalidMessage) {
		t.Fatal("un fallo de la base es transitorio: no debe clasificarse como mensaje inválido")
	}
}

// La entrega es at-least-once: repetir el mismo user.deleted termina igual que
// entregarlo una vez, sin error y sin tocar a nadie más.
func TestUserDeletedProcessorReentregaDelMismoEventoEsIdempotente(t *testing.T) {
	userID, otro := uuid.New(), uuid.New()
	quemador := nuevoQuemador(map[uuid.UUID]int64{userID: 3, otro: 2})
	dlq := &fakeDeadLetterPublisher{}
	handler := NewMessageHandler(NewUserDeletedProcessor(quemador), dlq, testQueue, testMaxRetries)
	body := cuerpoDeBaja(t, sobreDeBaja(t, actorDe(userID), payloadDeBaja()))

	for entrega := 1; entrega <= 3; entrega++ {
		if outcome := handler.Handle(context.Background(), body, 0); outcome != OutcomeAck {
			t.Fatalf("entrega %d: outcome = %v, esperaba OutcomeAck", entrega, outcome)
		}
	}

	if len(quemador.llamadas) != 3 {
		t.Fatalf("el procesador debía correr en cada entrega, corrió %d veces", len(quemador.llamadas))
	}
	if _, quedan := quemador.codigos[userID]; quedan || quemador.codigos[otro] != 2 {
		t.Errorf("estado final = %v, esperaba solo los códigos de otro usuario", quemador.codigos)
	}
	if len(dlq.published) != 0 {
		t.Errorf("una reentrega no debe ir a la DLQ: DLQ=%d", len(dlq.published))
	}
}

func TestUserDeletedProcessorConElMessageHandlerClasificacion(t *testing.T) {
	t.Run("actor inválido va directo a la DLQ sin reintento", func(t *testing.T) {
		quemador := nuevoQuemador(nil)
		dlq := &fakeDeadLetterPublisher{}
		handler := NewMessageHandler(NewUserDeletedProcessor(quemador), dlq, testQueue, testMaxRetries)
		noEsUUID := "abc"

		outcome := handler.Handle(context.Background(), cuerpoDeBaja(t, sobreDeBaja(t, &noEsUUID, payloadDeBaja())), 0)

		if outcome != OutcomeAck || len(dlq.published) != 1 || len(quemador.llamadas) != 0 {
			t.Fatalf("outcome = %v, DLQ = %d, llamadas = %d; esperaba ack tras publicar en la DLQ sin ejecutar nada", outcome, len(dlq.published), len(quemador.llamadas))
		}
		if dlq.published[0].queue != testQueue {
			t.Errorf("la DLQ se rutea por la cola de trabajo: queue = %q, esperaba %q", dlq.published[0].queue, testQueue)
		}
	})

	t.Run("envelope ilegible va directo a la DLQ sin reintento", func(t *testing.T) {
		quemador := nuevoQuemador(nil)
		dlq := &fakeDeadLetterPublisher{}
		handler := NewMessageHandler(NewUserDeletedProcessor(quemador), dlq, testQueue, testMaxRetries)

		outcome := handler.Handle(context.Background(), []byte(`no es un envelope`), 0)

		if outcome != OutcomeAck || len(dlq.published) != 1 || len(quemador.llamadas) != 0 {
			t.Fatalf("outcome = %v, DLQ = %d, llamadas = %d", outcome, len(dlq.published), len(quemador.llamadas))
		}
	})

	t.Run("fallo de la base se reintenta", func(t *testing.T) {
		quemador := nuevoQuemador(nil)
		quemador.err = errors.New("base de datos caída")
		dlq := &fakeDeadLetterPublisher{}
		handler := NewMessageHandler(NewUserDeletedProcessor(quemador), dlq, testQueue, testMaxRetries)

		outcome := handler.Handle(context.Background(), cuerpoDeBaja(t, sobreDeBaja(t, actorDe(uuid.New()), payloadDeBaja())), 1)

		if outcome != OutcomeNackNoRequeue || len(dlq.published) != 0 {
			t.Fatalf("outcome = %v, DLQ = %d; esperaba nack sin DLQ", outcome, len(dlq.published))
		}
	})

	t.Run("agotados los reintentos va a la DLQ", func(t *testing.T) {
		quemador := nuevoQuemador(nil)
		quemador.err = errors.New("base de datos caída")
		dlq := &fakeDeadLetterPublisher{}
		handler := NewMessageHandler(NewUserDeletedProcessor(quemador), dlq, testQueue, testMaxRetries)

		outcome := handler.Handle(context.Background(), cuerpoDeBaja(t, sobreDeBaja(t, actorDe(uuid.New()), payloadDeBaja())), testMaxRetries)

		if outcome != OutcomeAck || len(dlq.published) != 1 {
			t.Fatalf("outcome = %v, DLQ = %d; esperaba DLQ y ack al agotar MAX_RETRIES", outcome, len(dlq.published))
		}
	})
}
