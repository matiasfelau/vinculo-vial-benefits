//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

// brokerURL devuelve el broker de pruebas; sin TEST_RABBITMQ_URL la prueba se omite.
func brokerURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("TEST_RABBITMQ_URL no configurada: se omite la prueba de integración con RabbitMQ")
	}
	return url
}

// declareQueue declara la cola de trabajo del consumidor y la borra al terminar. La
// topología real (retry y DLQ) la declara Core; para esta prueba alcanza la cola.
func declareQueue(t *testing.T, url, queue string) *amqp.Channel {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("no se pudo conectar al broker de prueba: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("no se pudo abrir el canal: %v", err)
	}
	if _, err := ch.QueueDeclare(queue, false, true, false, false, nil); err != nil {
		t.Fatalf("no se pudo declarar la cola: %v", err)
	}
	return ch
}

func userDeletedBody(t *testing.T, userID uuid.UUID) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"eventId":   uuid.NewString(),
		"eventType": "user.deleted",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"producer":  "vinculo-vial-identity",
		"actor":     userID.String(),
		"payload":   map[string]any{"deleted_at": time.Now().UTC().Format(time.RFC3339)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// E2E 6: la baja de un usuario con código asignado pasa ese código a BURNED, y las
// estadísticas lo reflejan. Con el broker real, el consumidor del servicio y la base real.
func TestRunConElBrokerRealLaBajaDeUnUsuarioQuemaSuCodigo(t *testing.T) {
	cfg := testConfig(t, brokerURL(t))
	ch := declareQueue(t, cfg.RabbitMQURL.Reveal(), cfg.UserDeletedQueue)
	svc := startService(t, cfg)
	campaign := svc.createCampaign(t, nil)
	svc.uploadCodes(t, campaign, "BAJAS-0001\nBAJAS-0002\nBAJAS-0003")

	baja, headersBaja := user()
	_, headersOtro := user()
	codeBaja := claimedCode(t, svc.claim(t, campaign, headersBaja))
	codeOtro := claimedCode(t, svc.claim(t, campaign, headersOtro))

	if !eventually(func() bool {
		resp := send(t, http.MethodGet, svc.public+"/health/deps", nil, nil)
		return resp.status == http.StatusOK
	}) {
		t.Fatal("el consumidor no se conectó al broker: /health/deps no quedó en 200")
	}

	publish := func() {
		err := ch.PublishWithContext(context.Background(), "", cfg.UserDeletedQueue, false, false,
			amqp.Publishing{ContentType: "application/json", Body: userDeletedBody(t, baja)})
		if err != nil {
			t.Fatalf("no se pudo publicar user.deleted: %v", err)
		}
	}
	publish()

	if !eventually(func() bool { return svc.stats(t, campaign)["burned"] == 1 }) {
		t.Fatalf("el código del usuario dado de baja no pasó a BURNED: %v", svc.stats(t, campaign))
	}
	stats := svc.stats(t, campaign)
	if stats["assigned"] != 1 || stats["available"] != 1 || stats["burned"] != 1 {
		t.Errorf("stats = %v; el quemado no vuelve al pool y el del otro usuario sigue asignado", stats)
	}

	// La reentrega del mismo evento no cambia el resultado.
	publish()
	time.Sleep(300 * time.Millisecond)
	if again := svc.stats(t, campaign); again["burned"] != 1 || again["assigned"] != 1 || again["available"] != 1 {
		t.Errorf("tras la reentrega stats = %v", again)
	}

	// El otro usuario conserva su código; un usuario nuevo recibe uno distinto del quemado.
	if still := claimedCode(t, svc.claim(t, campaign, headersOtro)); still != codeOtro {
		t.Errorf("el otro usuario ve %q, esperaba %q", still, codeOtro)
	}
	_, headersNuevo := user()
	if nuevo := claimedCode(t, svc.claim(t, campaign, headersNuevo)); nuevo == codeBaja || nuevo == codeOtro {
		t.Errorf("el código quemado %q no debe volver al pool (recibió %q)", codeBaja, nuevo)
	}

	if strings.Contains(string(userDeletedBody(t, baja)), "BAJAS-") {
		t.Fatal("el evento de baja nunca lleva códigos")
	}
}
