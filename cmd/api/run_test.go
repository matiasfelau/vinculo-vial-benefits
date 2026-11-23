package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/crypto/codes"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/delivery/events"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/repository/postgres"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/seed"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/testutil/pgtest"
	"github.com/matiasfelau/vinculo-vial-benefits/pkg/config"
)

const (
	testAdminKey = "clave-de-administracion-de-pruebas-0123456789"
	// unreachableBroker es una URL de RabbitMQ a la que nadie escucha: el servicio arranca
	// igual y el consumidor reintenta con backoff.
	unreachableBroker = "amqp://guest:guest@127.0.0.1:1/"
	waitFor           = 20 * time.Second
)

func testKey(seed byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// freePort devuelve un puerto TCP libre del loopback.
func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("no se pudo reservar un puerto: %v", err)
	}
	defer func() { _ = listener.Close() }()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// testConfig arma una configuración de prueba sobre un esquema propio.
func testConfig(t *testing.T, brokerURL string) *config.Config {
	t.Helper()
	return &config.Config{
		Env:                    config.EnvDevelopment,
		Port:                   freePort(t),
		AdminPort:              freePort(t),
		AdminAPIKey:            config.Secret(testAdminKey),
		DatabaseURL:            config.Secret(pgtest.NewSchemaDSN(t)),
		DBQueryTimeout:         15 * time.Second,
		AdminDBTimeout:         30 * time.Second,
		RabbitMQURL:            config.Secret(brokerURL),
		RabbitMQPublishTimeout: 2 * time.Second,
		UserDeletedQueue:       "benefits.test-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] + ".q",
		RabbitMQPrefetch:       5,
		MaxRetries:             2,
		CodeAESKey:             config.Secret(testKey(1)),
		CodeHMACKey:            config.Secret(testKey(100)),
		BannerMaxBytes:         300 * 1024,
	}
}

// service es una instancia de run en marcha dentro de la prueba.
type service struct {
	cfg    *config.Config
	public string
	admin  string
	stop   func() error
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// startService arranca run y espera a que el servidor público responda.
func startService(t *testing.T, cfg *config.Config) *service {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, quietLogger()) }()

	svc := &service{cfg: cfg, public: "http://127.0.0.1:" + cfg.Port, admin: "http://127.0.0.1:" + cfg.AdminPort}
	var stopped bool
	svc.stop = func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(waitFor):
			return fmt.Errorf("run no terminó a tiempo tras cancelar el contexto")
		}
	}
	t.Cleanup(func() { _ = svc.stop() })

	if !eventually(func() bool {
		resp, err := http.Get(svc.public + "/health")
		if err != nil {
			select {
			case runErr := <-done:
				t.Fatalf("run terminó antes de servir: %v", runErr)
			default:
			}
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}) {
		t.Fatal("el servicio no empezó a responder")
	}
	return svc
}

func eventually(cond func() bool) bool {
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// reply es una respuesta HTTP leída por completo.
type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r reply) json(t *testing.T, dst any) {
	t.Helper()
	if err := json.Unmarshal(r.body, dst); err != nil {
		t.Fatalf("el cuerpo no es JSON (%d): %q (%v)", r.status, r.body, err)
	}
}

func (r reply) errCode(t *testing.T) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	r.json(t, &body)
	return body.Code
}

func send(t *testing.T, method, url string, headers map[string]string, body io.Reader) reply {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: data}
}

func user() (id uuid.UUID, headers map[string]string) {
	id = uuid.New()
	return id, map[string]string{"X-User-Id": id.String()}
}

func adminHeaders(extra ...string) map[string]string {
	headers := map[string]string{"X-Admin-Api-Key": testAdminKey}
	for i := 0; i+1 < len(extra); i += 2 {
		headers[extra[i]] = extra[i+1]
	}
	return headers
}

// createCampaign da de alta una campaña vigente por el router de administración.
func (s *service) createCampaign(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	fields := map[string]any{
		"partner_name": "Petrolera de prueba",
		"title":        "10 % de descuento",
		"description":  "Descuento en combustible",
		"terms":        "Un código por persona",
		"open_url":     "https://partner.example.com/canje",
		"starts_at":    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
		"active":       true,
	}
	if mutate != nil {
		mutate(fields)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for _, part := range []struct{ name, filename, content string }{
		{"campaign", "campaign.json", string(raw)},
		{"banner", "banner.png", string(seed.GenerateBanner(bannerColor()))},
	} {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, part.name, part.filename))
		w, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(part.content))
	}
	_ = writer.Close()

	resp := send(t, http.MethodPost, s.admin+"/admin/v1/campaigns", adminHeaders("Content-Type", writer.FormDataContentType()), body)
	if resp.status != http.StatusCreated {
		t.Fatalf("no se pudo crear la campaña: %d %s", resp.status, resp.body)
	}
	var created struct {
		ID string `json:"id"`
	}
	resp.json(t, &created)
	return created.ID
}

func (s *service) uploadCodes(t *testing.T, campaignID, csv string) map[string]int {
	t.Helper()
	resp := send(t, http.MethodPost, s.admin+"/admin/v1/campaigns/"+campaignID+"/codes", adminHeaders("Content-Type", "text/csv"), strings.NewReader(csv))
	if resp.status != http.StatusOK {
		t.Fatalf("no se pudieron cargar los códigos: %d %s", resp.status, resp.body)
	}
	var result map[string]int
	resp.json(t, &result)
	return result
}

func (s *service) claim(t *testing.T, campaignID string, headers map[string]string) reply {
	t.Helper()
	return send(t, http.MethodPost, s.public+"/api/v1/benefits/"+campaignID+"/claim", headers, nil)
}

func claimedCode(t *testing.T, r reply) string {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("claim = %d %s, esperaba 200", r.status, r.body)
	}
	var body struct {
		Code string `json:"code"`
	}
	r.json(t, &body)
	return body.Code
}

func (s *service) stats(t *testing.T, campaignID string) map[string]int {
	t.Helper()
	resp := send(t, http.MethodGet, s.admin+"/admin/v1/campaigns/"+campaignID+"/stats", adminHeaders(), nil)
	if resp.status != http.StatusOK {
		t.Fatalf("stats = %d %s", resp.status, resp.body)
	}
	var stats map[string]int
	resp.json(t, &stats)
	return stats
}

func (s *service) benefitIDs(t *testing.T, headers map[string]string) []string {
	t.Helper()
	resp := send(t, http.MethodGet, s.public+"/api/v1/benefits", headers, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("list = %d %s", resp.status, resp.body)
	}
	var items []struct {
		ID   string  `json:"id"`
		Code *string `json:"code"`
	}
	resp.json(t, &items)
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// El flujo del contrato con la base real: carga de códigos, claim idempotente, dos
// usuarios distintos, pool agotado, canjes y estadísticas.
func TestRunSirveElContratoCompletoSobreLaBaseReal(t *testing.T) {
	svc := startService(t, testConfig(t, unreachableBroker))
	campaign := svc.createCampaign(t, nil)

	loaded := svc.uploadCodes(t, campaign, "TESTE-0001\r\nTESTE-0002\nmal código\nTESTE-0001\n")
	if loaded["inserted"] != 2 || loaded["duplicates"] != 1 || loaded["invalid"] != 1 {
		t.Fatalf("carga = %v, esperaba 2 insertados, 1 duplicado y 1 inválido", loaded)
	}

	// E2E 2: "Obtener código" devuelve un código y, al volver, el mismo.
	_, headersA := user()
	codeA := claimedCode(t, svc.claim(t, campaign, headersA))
	if again := claimedCode(t, svc.claim(t, campaign, headersA)); again != codeA {
		t.Fatalf("el segundo pedido devolvió %q, esperaba %q", again, codeA)
	}
	if !strings.HasPrefix(codeA, "TESTE-000") {
		t.Errorf("el código descifrado = %q; debe ser el que se cargó", codeA)
	}

	// E2E 3: dos usuarios en la misma campaña reciben códigos distintos.
	_, headersB := user()
	codeB := claimedCode(t, svc.claim(t, campaign, headersB))
	if codeB == codeA {
		t.Fatalf("dos usuarios recibieron el mismo código %q", codeA)
	}

	// E2E 4: con el pool agotado, un usuario nuevo recibe NO_CODES_LEFT y el que ya tenía código lo sigue viendo.
	_, headersC := user()
	sinCodigo := svc.claim(t, campaign, headersC)
	if sinCodigo.status != http.StatusConflict || sinCodigo.errCode(t) != "NO_CODES_LEFT" {
		t.Fatalf("claim con el pool agotado = %d %s", sinCodigo.status, sinCodigo.body)
	}
	if again := claimedCode(t, svc.claim(t, campaign, headersA)); again != codeA {
		t.Errorf("el que ya tenía código ve %q, esperaba %q", again, codeA)
	}
	if contains(svc.benefitIDs(t, headersC), campaign) {
		t.Error("la campaña agotada no debe listarse para quien no tiene código")
	}
	list := send(t, http.MethodGet, svc.public+"/api/v1/benefits", headersA, nil)
	var items []map[string]any
	list.json(t, &items)
	if len(items) != 1 || items[0]["code"] != codeA {
		t.Errorf("el listado de quien tiene código = %s", list.body)
	}

	// E2E 7: reporte de canjes con un código válido y uno inexistente.
	redemptions := send(t, http.MethodPost, svc.admin+"/admin/v1/campaigns/"+campaign+"/redemptions",
		adminHeaders("Content-Type", "text/csv"), strings.NewReader(codeA+"\nNOEXISTE-9999\n"))
	var matched map[string]int
	redemptions.json(t, &matched)
	if redemptions.status != http.StatusOK || matched["matched"] != 1 || matched["unknown"] != 1 {
		t.Fatalf("canjes = %d %s, esperaba matched 1 y unknown 1", redemptions.status, redemptions.body)
	}

	stats := svc.stats(t, campaign)
	if stats["available"] != 0 || stats["assigned"] != 2 || stats["burned"] != 0 || stats["redeemed"] != 1 {
		t.Errorf("stats = %v", stats)
	}
}

func TestRunElBannerSeSirveConEtagYCache(t *testing.T) {
	svc := startService(t, testConfig(t, unreachableBroker))
	campaign := svc.createCampaign(t, nil)
	svc.uploadCodes(t, campaign, "BANNR-0001")
	_, headers := user()

	first := send(t, http.MethodGet, svc.public+"/api/v1/benefits/"+campaign+"/banner", headers, nil)

	if first.status != http.StatusOK || first.header.Get("Content-Type") != "image/png" ||
		first.header.Get("Cache-Control") != "private, max-age=3600" || !bytes.HasPrefix(first.body, []byte("\x89PNG")) {
		t.Fatalf("banner = %d %v (%d bytes)", first.status, first.header, len(first.body))
	}
	etag := first.header.Get("ETag")
	if !strings.HasPrefix(etag, `"`) || len(etag) != 66 {
		t.Fatalf("ETag = %q, esperaba el SHA-256 entre comillas", etag)
	}
	second := send(t, http.MethodGet, svc.public+"/api/v1/benefits/"+campaign+"/banner", map[string]string{"X-User-Id": headers["X-User-Id"], "If-None-Match": etag}, nil)
	if second.status != http.StatusNotModified || len(second.body) != 0 {
		t.Errorf("con If-None-Match = %d (%d bytes), esperaba 304 sin cuerpo", second.status, len(second.body))
	}
	missing := send(t, http.MethodGet, svc.public+"/api/v1/benefits/"+uuid.NewString()+"/banner", headers, nil)
	if missing.status != http.StatusNotFound || missing.errCode(t) != "BENEFIT_NOT_FOUND" {
		t.Errorf("banner inexistente = %d %s", missing.status, missing.body)
	}
}

func TestRunUnaCampañaVencidaOInactivaNoSeMuestraNiSeReclama(t *testing.T) {
	svc := startService(t, testConfig(t, unreachableBroker))
	vencida := svc.createCampaign(t, func(f map[string]any) {
		f["starts_at"] = time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
		f["ends_at"] = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	})
	inactiva := svc.createCampaign(t, func(f map[string]any) { f["active"] = false })
	vigente := svc.createCampaign(t, nil)
	for _, id := range []string{vencida, inactiva, vigente} {
		svc.uploadCodes(t, id, "VIGEN-"+id[:8])
	}
	_, headers := user()

	ids := svc.benefitIDs(t, headers)

	if len(ids) != 1 || ids[0] != vigente {
		t.Fatalf("ids = %v, esperaba solo la vigente %s", ids, vigente)
	}
	for nombre, id := range map[string]string{"vencida": vencida, "inactiva": inactiva} {
		resp := svc.claim(t, id, headers)
		if resp.status != http.StatusNotFound || resp.errCode(t) != "BENEFIT_NOT_FOUND" {
			t.Errorf("claim de la campaña %s = %d %s, esperaba 404 BENEFIT_NOT_FOUND", nombre, resp.status, resp.body)
		}
	}

	// Activar la inactiva por PATCH la hace visible.
	patch := send(t, http.MethodPatch, svc.admin+"/admin/v1/campaigns/"+inactiva, adminHeaders(), strings.NewReader(`{"active":true}`))
	if patch.status != http.StatusNoContent {
		t.Fatalf("patch = %d %s", patch.status, patch.body)
	}
	if ids := svc.benefitIDs(t, headers); len(ids) != 2 || !contains(ids, inactiva) {
		t.Errorf("tras activarla ids = %v", ids)
	}
}

// E2E 8: /admin/* sin clave es 401, y el router público no los sirve.
func TestRunLasRutasDeAdministracionPidenClaveYNoEstanEnElRouterPublico(t *testing.T) {
	svc := startService(t, testConfig(t, unreachableBroker))
	campaign := svc.createCampaign(t, nil)
	rutas := []struct{ method, path string }{
		{http.MethodGet, "/admin/v1/campaigns"},
		{http.MethodPost, "/admin/v1/campaigns"},
		{http.MethodPatch, "/admin/v1/campaigns/" + campaign},
		{http.MethodPut, "/admin/v1/campaigns/" + campaign + "/banner"},
		{http.MethodPost, "/admin/v1/campaigns/" + campaign + "/codes"},
		{http.MethodPost, "/admin/v1/campaigns/" + campaign + "/redemptions"},
		{http.MethodGet, "/admin/v1/campaigns/" + campaign + "/stats"},
		{http.MethodGet, "/admin/v1/no-existe"},
	}

	for _, ruta := range rutas {
		sinClave := send(t, ruta.method, svc.admin+ruta.path, nil, nil)
		claveMala := send(t, ruta.method, svc.admin+ruta.path, map[string]string{"X-Admin-Api-Key": "incorrecta"}, nil)
		enPublico := send(t, ruta.method, svc.public+ruta.path, adminHeaders("X-User-Id", uuid.NewString()), nil)

		if sinClave.status != http.StatusUnauthorized || claveMala.status != http.StatusUnauthorized {
			t.Errorf("%s %s: sin clave = %d y clave incorrecta = %d, esperaba 401", ruta.method, ruta.path, sinClave.status, claveMala.status)
		}
		if enPublico.status != http.StatusNotFound {
			t.Errorf("%s %s en el router público = %d, esperaba 404", ruta.method, ruta.path, enPublico.status)
		}
	}
}

func TestRunSinXUserIdLasRutasPublicasDan401(t *testing.T) {
	svc := startService(t, testConfig(t, unreachableBroker))

	resp := send(t, http.MethodGet, svc.public+"/api/v1/benefits", nil, nil)

	if resp.status != http.StatusUnauthorized || resp.errCode(t) != "UNAUTHORIZED" {
		t.Fatalf("respuesta = %d %s", resp.status, resp.body)
	}
}

func TestRunHealthDepsReportaElConsumidorCaidoYReadyLaBase(t *testing.T) {
	svc := startService(t, testConfig(t, unreachableBroker))

	ready := send(t, http.MethodGet, svc.public+"/ready", nil, nil)
	deps := send(t, http.MethodGet, svc.public+"/health/deps", nil, nil)

	if ready.status != http.StatusOK {
		t.Errorf("/ready = %d %s", ready.status, ready.body)
	}
	if deps.status != http.StatusServiceUnavailable || !strings.Contains(string(deps.body), `"user_deleted_consumer":"down"`) {
		t.Errorf("/health/deps = %d %s; con el broker caído el consumidor figura down", deps.status, deps.body)
	}
	// Con el broker caído el servicio sigue sirviendo las rutas públicas.
	_, headers := user()
	if list := send(t, http.MethodGet, svc.public+"/api/v1/benefits", headers, nil); list.status != http.StatusOK {
		t.Errorf("list con el broker caído = %d", list.status)
	}
}

func TestRunSinClaveDeAdministracionNoArrancaElRouterDeAdministracion(t *testing.T) {
	cfg := testConfig(t, unreachableBroker)
	cfg.AdminAPIKey = ""
	svc := startService(t, cfg)

	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+cfg.AdminPort, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("el puerto de administración no debía abrirse sin ADMIN_API_KEY")
	}
	if err := svc.stop(); err != nil {
		t.Errorf("el apagado debía ser limpio: %v", err)
	}
}

func TestRunSeApagaOrdenadamenteAlCancelarElContexto(t *testing.T) {
	svc := startService(t, testConfig(t, unreachableBroker))

	if err := svc.stop(); err != nil {
		t.Fatalf("run devolvió %v tras un apagado ordenado", err)
	}
	if _, err := http.Get(svc.public + "/health"); err == nil {
		t.Error("tras el apagado el puerto público no debe seguir sirviendo")
	}
}

func TestRunFallaSiLasClavesDeLosCodigosSonInvalidas(t *testing.T) {
	cfg := testConfig(t, unreachableBroker)
	cfg.CodeAESKey = "no-es-base64"

	if err := run(context.Background(), cfg, quietLogger()); err == nil || strings.Contains(err.Error(), "no-es-base64") {
		t.Fatalf("run = %v, esperaba un error que no refleje la clave", err)
	}
}

func TestRunFallaSiLaBaseNoEstaDisponible(t *testing.T) {
	cfg := testConfig(t, unreachableBroker)
	cfg.DatabaseURL = "postgres://nadie:nada@127.0.0.1:1/nada?sslmode=disable&connect_timeout=1"

	if err := run(context.Background(), cfg, quietLogger()); err == nil {
		t.Fatal("run debía fallar sin base de datos")
	}
}

func TestRunFallaSiUnPuertoEstaOcupado(t *testing.T) {
	cfg := testConfig(t, unreachableBroker)
	ocupado, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		t.Fatalf("no se pudo ocupar el puerto: %v", err)
	}
	defer func() { _ = ocupado.Close() }()

	done := make(chan error, 1)
	go func() { done <- run(context.Background(), cfg, quietLogger()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run debía fallar con el puerto ocupado")
		}
	case <-time.After(waitFor):
		t.Fatal("run no terminó con el puerto ocupado")
	}
}

func TestBuildServersRechazaUnaClaveDeAdministracionDebil(t *testing.T) {
	cfg := testConfig(t, unreachableBroker)
	cfg.AdminAPIKey = "corta"
	protector, err := codes.NewProtector(cfg.CodeAESKey.Reveal(), cfg.CodeHMACKey.Reveal())
	if err != nil {
		t.Fatal(err)
	}

	servers, err := buildServers(cfg, quietLogger(), (*postgres.Repository)(nil), protector, &events.ConnectionStatus{})

	if err == nil || servers != nil {
		t.Fatalf("buildServers = %v, %v; esperaba un error", servers, err)
	}
}

func bannerColor() color.RGBA { return color.RGBA{R: 10, G: 120, B: 200, A: 255} }

func TestExecuteConUnaConfiguracionInvalidaDevuelveCodigoDeError(t *testing.T) {
	load := func() (*config.Config, error) { return nil, fmt.Errorf("config: DATABASE_URL es obligatoria") }

	if code := execute(context.Background(), load, quietLogger()); code != exitError {
		t.Fatalf("código de salida = %d, esperaba %d", code, exitError)
	}
}

func TestExecuteSiRunFallaDevuelveCodigoDeError(t *testing.T) {
	cfg := testConfig(t, unreachableBroker)
	cfg.CodeHMACKey = cfg.CodeAESKey // claves no independientes: run no arranca

	if code := execute(context.Background(), func() (*config.Config, error) { return cfg, nil }, quietLogger()); code != exitError {
		t.Fatalf("código de salida = %d, esperaba %d", code, exitError)
	}
}

func TestExecuteTrasUnApagadoOrdenadoDevuelveCero(t *testing.T) {
	cfg := testConfig(t, unreachableBroker)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- execute(ctx, func() (*config.Config, error) { return cfg, nil }, quietLogger()) }()

	if !eventually(func() bool {
		resp, err := http.Get("http://127.0.0.1:" + cfg.Port + "/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}) {
		t.Fatal("el servicio no empezó a responder")
	}
	cancel()

	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("código de salida = %d, esperaba %d", code, exitOK)
		}
	case <-time.After(waitFor):
		t.Fatal("execute no terminó tras cancelar el contexto")
	}
}
