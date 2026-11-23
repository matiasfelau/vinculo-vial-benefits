package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/service/benefit"
)

var testUser = uuid.MustParse("11111111-1111-4111-8111-111111111111")

// fakeBenefits implementa BenefitService con resultados configurables.
type fakeBenefits struct {
	views       []benefit.View
	listErr     error
	code        string
	claimErr    error
	banner      domain.Banner
	notModified bool
	bannerErr   error
	doPanic     bool

	gotUser        uuid.UUID
	gotCampaign    uuid.UUID
	gotIfNoneMatch string
}

func (f *fakeBenefits) List(_ context.Context, userID uuid.UUID) ([]benefit.View, error) {
	if f.doPanic {
		panic("fallo inesperado SECRETO-PANIC")
	}
	f.gotUser = userID
	return f.views, f.listErr
}

func (f *fakeBenefits) Claim(_ context.Context, campaignID, userID uuid.UUID) (string, error) {
	f.gotCampaign, f.gotUser = campaignID, userID
	return f.code, f.claimErr
}

func (f *fakeBenefits) Banner(_ context.Context, campaignID uuid.UUID, ifNoneMatch string) (domain.Banner, bool, error) {
	f.gotCampaign, f.gotIfNoneMatch = campaignID, ifNoneMatch
	return f.banner, f.notModified, f.bannerErr
}

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// newPublic arma el router público con un logger que escribe en logs.
func newPublic(t *testing.T, svc BenefitService, db Pingable, deps ...DependencyCheck) (http.Handler, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	return NewRouter(RouterConfig{Benefits: svc, DB: db, Deps: deps, Logger: logger}), logs
}

// call ejecuta una petición contra el handler y devuelve la respuesta grabada.
func call(handler http.Handler, method, path string, headers map[string]string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func asUser(extra ...string) map[string]string {
	headers := map[string]string{HeaderUserID: testUser.String()}
	for i := 0; i+1 < len(extra); i += 2 {
		headers[extra[i]] = extra[i+1]
	}
	return headers
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("la respuesta no es un error { error, code }: %q (%v)", rec.Body.String(), err)
	}
	return body
}

// --- Salud -----------------------------------------------------------------

func TestHealthYReadyNoPidenIdentidad(t *testing.T) {
	router, _ := newPublic(t, &fakeBenefits{}, fakePinger{})

	if rec := call(router, http.MethodGet, "/health", nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Errorf("/health = %d %s", rec.Code, rec.Body)
	}
	if rec := call(router, http.MethodGet, "/ready", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("/ready = %d %s", rec.Code, rec.Body)
	}
}

func TestReadyConLaBaseCaidaEs503(t *testing.T) {
	router, logs := newPublic(t, &fakeBenefits{}, fakePinger{err: errors.New("sin conexión")})

	rec := call(router, http.MethodGet, "/ready", nil, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ready = %d, esperaba 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sin conexión") {
		t.Errorf("la respuesta no debe exponer el detalle interno: %s", rec.Body)
	}
	if !strings.Contains(logs.String(), "base de datos inalcanzable") {
		t.Error("la caída de la base debe quedar en el log")
	}
}

func TestHealthDepsReportaCadaDependencia(t *testing.T) {
	arriba := DependencyCheck{Name: "user_deleted_consumer", Healthy: func() bool { return true }}
	caida := DependencyCheck{Name: "otra", Healthy: func() bool { return false }}

	ok, _ := newPublic(t, &fakeBenefits{}, fakePinger{}, arriba)
	degradado, _ := newPublic(t, &fakeBenefits{}, fakePinger{}, arriba, caida)
	sinChequeos, _ := newPublic(t, &fakeBenefits{}, fakePinger{})

	recOK := call(ok, http.MethodGet, "/health/deps", nil, nil)
	recDegradado := call(degradado, http.MethodGet, "/health/deps", nil, nil)
	recVacio := call(sinChequeos, http.MethodGet, "/health/deps", nil, nil)

	if recOK.Code != http.StatusOK || !strings.Contains(recOK.Body.String(), `"user_deleted_consumer":"up"`) {
		t.Errorf("con todo arriba = %d %s", recOK.Code, recOK.Body)
	}
	if recDegradado.Code != http.StatusServiceUnavailable || !strings.Contains(recDegradado.Body.String(), `"degraded"`) ||
		!strings.Contains(recDegradado.Body.String(), `"otra":"down"`) {
		t.Errorf("con una caída = %d %s", recDegradado.Code, recDegradado.Body)
	}
	if recVacio.Code != http.StatusOK {
		t.Errorf("sin dependencias = %d", recVacio.Code)
	}
}

// --- Identidad -------------------------------------------------------------

func TestLasRutasPublicasExigenUnXUserIdValido(t *testing.T) {
	svc := &fakeBenefits{}
	router, logs := newPublic(t, svc, fakePinger{})
	rutas := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/benefits"},
		{http.MethodGet, "/api/v1/benefits/" + uuid.NewString() + "/banner"},
		{http.MethodPost, "/api/v1/benefits/" + uuid.NewString() + "/claim"},
	}
	invalidos := map[string]map[string]string{
		"ausente":       nil,
		"vacío":         {HeaderUserID: ""},
		"no es UUID":    {HeaderUserID: "admin"},
		"UUID nulo":     {HeaderUserID: uuid.Nil.String()},
		"inyección SQL": {HeaderUserID: "1' OR '1'='1"},
	}

	for _, ruta := range rutas {
		for nombre, headers := range invalidos {
			t.Run(ruta.method+" "+nombre, func(t *testing.T) {
				rec := call(router, ruta.method, ruta.path, headers, nil)

				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, esperaba 401", rec.Code)
				}
				if body := errorOf(t, rec); body.Code != "UNAUTHORIZED" {
					t.Errorf("code = %q, esperaba UNAUTHORIZED", body.Code)
				}
			})
		}
	}
	if svc.gotUser != uuid.Nil {
		t.Error("una petición sin identidad no debe llegar al caso de uso")
	}
	if !strings.Contains(logs.String(), "sin identidad de usuario válida") {
		t.Error("el fallo de autenticación debe quedar en el log")
	}
	if strings.Contains(logs.String(), "OR '1'='1") {
		t.Error("el log no debe reproducir el valor del encabezado")
	}
}

// --- Listado ---------------------------------------------------------------

func TestListDevuelveLasCampañasConSuBannerURLYCodigoNullSiNoLoPidio(t *testing.T) {
	con, sin := uuid.New(), uuid.New()
	codigo := "PROMO-0001"
	fin := time.Date(2026, 12, 31, 23, 59, 59, 0, time.FixedZone("ART", -3*3600))
	svc := &fakeBenefits{views: []benefit.View{
		{ID: con, PartnerName: "Petrolera", Title: "T1", Description: "D1", Terms: "C1", OpenURL: "https://p.example.com", EndsAt: fin, Code: &codigo},
		{ID: sin, PartnerName: "Otra", Title: "T2", Description: "D2", Terms: "C2", OpenURL: "https://o.example.com", EndsAt: fin.Add(time.Hour)},
	}}
	router, logs := newPublic(t, svc, fakePinger{})

	rec := call(router, http.MethodGet, "/api/v1/benefits", asUser(), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q; una lista con códigos no se cachea", got)
	}
	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil || len(items) != 2 {
		t.Fatalf("cuerpo = %s (%v)", rec.Body, err)
	}
	primero := items[0]
	if primero["id"] != con.String() || primero["partner_name"] != "Petrolera" || primero["title"] != "T1" ||
		primero["description"] != "D1" || primero["terms"] != "C1" || primero["open_url"] != "https://p.example.com" {
		t.Errorf("campos = %v", primero)
	}
	if primero["banner_url"] != "/api/v1/benefits/"+con.String()+"/banner" {
		t.Errorf("banner_url = %v", primero["banner_url"])
	}
	if primero["ends_at"] != "2027-01-01T02:59:59Z" {
		t.Errorf("ends_at = %v, esperaba RFC 3339 en UTC", primero["ends_at"])
	}
	if primero["code"] != codigo {
		t.Errorf("code = %v, esperaba %q", primero["code"], codigo)
	}
	if code, presente := items[1]["code"]; !presente || code != nil {
		t.Errorf("code = %v (presente %v); sin pedirlo debe ser null explícito", code, presente)
	}
	if svc.gotUser != testUser {
		t.Errorf("el caso de uso recibió el usuario %v, esperaba el de X-User-Id", svc.gotUser)
	}
	if strings.Contains(logs.String(), codigo) {
		t.Error("el log nunca debe contener un código")
	}
}

func TestListSinCampañasDevuelveUnArregloVacio(t *testing.T) {
	router, _ := newPublic(t, &fakeBenefits{}, fakePinger{})

	rec := call(router, http.MethodGet, "/api/v1/benefits", asUser(), nil)

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("respuesta = %d %q, esperaba 200 y []", rec.Code, rec.Body)
	}
}

// --- Banner ----------------------------------------------------------------

func TestBannerDevuelveLaImagenConSusEncabezados(t *testing.T) {
	id := uuid.New()
	svc := &fakeBenefits{banner: domain.Banner{ContentType: "image/png", ETag: "abc123", Data: []byte("PNGDATA")}}
	router, _ := newPublic(t, svc, fakePinger{})

	rec := call(router, http.MethodGet, "/api/v1/benefits/"+id.String()+"/banner", asUser(), nil)

	if rec.Code != http.StatusOK || rec.Body.String() != "PNGDATA" {
		t.Fatalf("respuesta = %d %q", rec.Code, rec.Body)
	}
	wantHeaders := map[string]string{
		"Content-Type":           "image/png",
		"ETag":                   `"abc123"`,
		"Cache-Control":          "private, max-age=3600",
		"X-Content-Type-Options": "nosniff",
	}
	for name, want := range wantHeaders {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, esperaba %q", name, got, want)
		}
	}
	if svc.gotCampaign != id {
		t.Errorf("campaña = %v, esperaba %v", svc.gotCampaign, id)
	}
}

func TestBannerConIfNoneMatchCoincidenteResponde304SinCuerpo(t *testing.T) {
	svc := &fakeBenefits{banner: domain.Banner{ContentType: "image/png", ETag: "abc123"}, notModified: true}
	router, _ := newPublic(t, svc, fakePinger{})

	rec := call(router, http.MethodGet, "/api/v1/benefits/"+uuid.NewString()+"/banner", asUser("If-None-Match", `"abc123"`), nil)

	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("respuesta = %d %q, esperaba 304 sin cuerpo", rec.Code, rec.Body)
	}
	if rec.Header().Get("ETag") != `"abc123"` || rec.Header().Get("Cache-Control") != "private, max-age=3600" {
		t.Errorf("el 304 debe repetir ETag y Cache-Control: %v", rec.Header())
	}
	if svc.gotIfNoneMatch != `"abc123"` {
		t.Errorf("If-None-Match recibido = %q", svc.gotIfNoneMatch)
	}
}

func TestBannerConUnIdQueNoEsUUIDEsBenefitNotFound(t *testing.T) {
	svc := &fakeBenefits{}
	router, _ := newPublic(t, svc, fakePinger{})

	rec := call(router, http.MethodGet, "/api/v1/benefits/no-es-uuid/banner", asUser(), nil)

	if rec.Code != http.StatusNotFound || errorOf(t, rec).Code != "BENEFIT_NOT_FOUND" {
		t.Fatalf("respuesta = %d %s", rec.Code, rec.Body)
	}
	if svc.gotCampaign != uuid.Nil {
		t.Error("un id inválido no debe llegar al caso de uso")
	}
}

// --- Claim -----------------------------------------------------------------

func TestClaimDevuelveElCodigoYNoLoLoguea(t *testing.T) {
	id := uuid.New()
	svc := &fakeBenefits{code: "PROMO-0001"}
	router, logs := newPublic(t, svc, fakePinger{})

	rec := call(router, http.MethodPost, "/api/v1/benefits/"+id.String()+"/claim", asUser(), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != "PROMO-0001" || len(body) != 1 {
		t.Errorf("cuerpo = %s", rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q; la respuesta con un código no se cachea", rec.Header().Get("Cache-Control"))
	}
	if svc.gotCampaign != id || svc.gotUser != testUser {
		t.Errorf("caso de uso recibió %v y %v", svc.gotCampaign, svc.gotUser)
	}
	if strings.Contains(logs.String(), "PROMO-0001") {
		t.Error("el log nunca debe contener un código")
	}
}

func TestLosErroresDeLosCasosDeUsoSeTraducenAlContrato(t *testing.T) {
	casos := map[string]struct {
		err    error
		status int
		code   string
	}{
		"campaña inexistente o no vigente": {domain.ErrBenefitNotFound, http.StatusNotFound, "BENEFIT_NOT_FOUND"},
		"sin códigos":                      {domain.ErrNoCodesLeft, http.StatusConflict, "NO_CODES_LEFT"},
		"base caída":                       {domain.ErrRepositoryUnavailable, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"},
		"error desconocido":                {errors.New("boom con SECRETO-INTERNO"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	rutas := map[string]struct {
		method string
		path   string
		config func(*fakeBenefits, error)
	}{
		"claim":  {http.MethodPost, "/claim", func(f *fakeBenefits, err error) { f.claimErr = err }},
		"list":   {http.MethodGet, "", func(f *fakeBenefits, err error) { f.listErr = err }},
		"banner": {http.MethodGet, "/banner", func(f *fakeBenefits, err error) { f.bannerErr = err }},
	}

	for rutaNombre, ruta := range rutas {
		for nombre, caso := range casos {
			t.Run(rutaNombre+" "+nombre, func(t *testing.T) {
				svc := &fakeBenefits{}
				ruta.config(svc, caso.err)
				router, logs := newPublic(t, svc, fakePinger{})
				path := "/api/v1/benefits"
				if ruta.path != "" {
					path += "/" + uuid.NewString() + ruta.path
				}

				rec := call(router, ruta.method, path, asUser(), nil)

				if rec.Code != caso.status || errorOf(t, rec).Code != caso.code {
					t.Fatalf("respuesta = %d %s; esperaba %d %s", rec.Code, rec.Body, caso.status, caso.code)
				}
				if strings.Contains(rec.Body.String(), "SECRETO-INTERNO") {
					t.Errorf("el detalle interno no debe salir en la respuesta: %s", rec.Body)
				}
				if caso.status == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") != "5" {
					t.Errorf("Retry-After = %q, esperaba 5", rec.Header().Get("Retry-After"))
				}
				if caso.status == http.StatusInternalServerError && !strings.Contains(logs.String(), "SECRETO-INTERNO") {
					t.Error("la causa del 500 debe quedar en el log")
				}
			})
		}
	}
}

// --- Router ----------------------------------------------------------------

func TestUnaRutaInexistenteOUnMetodoIncorrectoResponden404Y405ConElFormatoDelContrato(t *testing.T) {
	router, _ := newPublic(t, &fakeBenefits{}, fakePinger{})

	notFound := call(router, http.MethodGet, "/api/v1/otra-cosa", asUser(), nil)
	wrongMethod := call(router, http.MethodDelete, "/api/v1/benefits", asUser(), nil)
	trailing := call(router, http.MethodGet, "/api/v1/benefits/", asUser(), nil)

	if notFound.Code != http.StatusNotFound || errorOf(t, notFound).Code != "NOT_FOUND" {
		t.Errorf("ruta inexistente = %d %s", notFound.Code, notFound.Body)
	}
	if wrongMethod.Code != http.StatusMethodNotAllowed || errorOf(t, wrongMethod).Code != "METHOD_NOT_ALLOWED" {
		t.Errorf("método incorrecto = %d %s", wrongMethod.Code, wrongMethod.Body)
	}
	if trailing.Code != http.StatusNotFound {
		t.Errorf("con barra final = %d; no se redirige, el path es exacto", trailing.Code)
	}
}

func TestLasRutasDeAdministracionNoExistenEnElRouterPublico(t *testing.T) {
	router, _ := newPublic(t, &fakeBenefits{}, fakePinger{})

	for _, path := range []string{"/admin/v1/campaigns", "/admin/v1/campaigns/" + uuid.NewString() + "/codes"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch} {
			if rec := call(router, method, path, asUser(HeaderAdminAPIKey, strings.Repeat("k", 40)), nil); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, esperaba 404: las rutas de carga y de reporte van solo en el router de administración", method, path, rec.Code)
			}
		}
	}
}

func TestRequestIDSePropagaOSeGenera(t *testing.T) {
	router, logs := newPublic(t, &fakeBenefits{}, fakePinger{})

	propagado := call(router, http.MethodGet, "/health", map[string]string{HeaderRequestID: "trace-123"}, nil)
	generado := call(router, http.MethodGet, "/health", nil, nil)
	desmedido := call(router, http.MethodGet, "/health", map[string]string{HeaderRequestID: strings.Repeat("x", 500)}, nil)

	if propagado.Header().Get(HeaderRequestID) != "trace-123" {
		t.Errorf("X-Request-ID = %q, esperaba el recibido", propagado.Header().Get(HeaderRequestID))
	}
	if _, err := uuid.Parse(generado.Header().Get(HeaderRequestID)); err != nil {
		t.Errorf("sin X-Request-ID debía generarse un UUID: %q", generado.Header().Get(HeaderRequestID))
	}
	if got := desmedido.Header().Get(HeaderRequestID); len(got) > maxRequestIDLength {
		t.Errorf("un X-Request-ID desmedido no debe propagarse (%d caracteres)", len(got))
	}
	if !strings.Contains(logs.String(), `"request_id":"trace-123"`) {
		t.Error("el access log debe llevar el request_id")
	}
}

func TestUnPanicSeConvierteEnUn500GenericoSinStack(t *testing.T) {
	router, logs := newPublic(t, &fakeBenefits{doPanic: true}, fakePinger{})

	rec := call(router, http.MethodGet, "/api/v1/benefits", asUser(), nil)

	if rec.Code != http.StatusInternalServerError || errorOf(t, rec).Code != "INTERNAL_ERROR" {
		t.Fatalf("respuesta = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "SECRETO-PANIC") || strings.Contains(rec.Body.String(), "goroutine") {
		t.Errorf("la respuesta no debe exponer el panic ni el stack: %s", rec.Body)
	}
	if !strings.Contains(logs.String(), "panic atendiendo la petición") {
		t.Error("el panic debe quedar en el log")
	}
}

func TestElAccessLogAcotaElPathYNoIncluyeLaQuery(t *testing.T) {
	router, logs := newPublic(t, &fakeBenefits{}, fakePinger{})

	call(router, http.MethodGet, "/"+strings.Repeat("a", 400)+"?code=SECRETO-QUERY", nil, nil)

	if strings.Contains(logs.String(), "SECRETO-QUERY") {
		t.Error("el log no debe incluir la query string")
	}
	if strings.Contains(logs.String(), strings.Repeat("a", 300)) {
		t.Error("el path del log debe estar acotado")
	}
}

func TestLimitBodyCortaLaLecturaAlSuperarElTope(t *testing.T) {
	engine := gin.New()
	engine.Use(limitBody(16))
	engine.POST("/", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	})

	chico := call(engine, http.MethodPost, "/", nil, strings.NewReader(strings.Repeat("a", 16)))
	grande := call(engine, http.MethodPost, "/", nil, strings.NewReader(strings.Repeat("a", 17)))

	if chico.Code != http.StatusOK || grande.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chico = %d y grande = %d; esperaba 200 y 413", chico.Code, grande.Code)
	}
}

func TestNewRouterSinLoggerUsaElPorDefecto(t *testing.T) {
	router := NewRouter(RouterConfig{Benefits: &fakeBenefits{}, DB: fakePinger{}})

	if rec := call(router, http.MethodGet, "/health", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}
