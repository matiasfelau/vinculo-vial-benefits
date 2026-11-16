package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/service/campaign"
)

const (
	testAdminKey   = "0123456789abcdef0123456789abcdef-clave-de-prueba"
	testBannerMax  = 2048
	campaignPath   = "/admin/v1/campaigns"
	validCampaign  = `{"partner_name":"Petrolera","title":"T","description":"D","terms":"C","open_url":"https://p.example.com","starts_at":"2026-11-01T00:00:00Z","ends_at":"2026-12-01T00:00:00Z","active":true}`
	pngMagicPrefix = "\x89PNG\r\n\x1a\n"
)

// fakeCampaigns implementa CampaignService con resultados configurables.
type fakeCampaigns struct {
	createdID uuid.UUID
	createErr error
	created   campaign.CreateInput

	summaries []domain.CampaignSummary
	listErr   error

	patchErr error
	patched  domain.CampaignPatch
	patchID  uuid.UUID

	replaceErr error
	replaced   []byte

	upload    domain.UploadResult
	uploadErr error
	uploadCSV string

	redeem    domain.RedemptionResult
	redeemErr error
	redeemCSV string

	stats    domain.CampaignStats
	statsErr error
}

func (f *fakeCampaigns) Create(_ context.Context, in campaign.CreateInput) (uuid.UUID, error) {
	f.created = in
	return f.createdID, f.createErr
}
func (f *fakeCampaigns) List(context.Context) ([]domain.CampaignSummary, error) {
	return f.summaries, f.listErr
}
func (f *fakeCampaigns) Patch(_ context.Context, id uuid.UUID, p domain.CampaignPatch) error {
	f.patchID, f.patched = id, p
	return f.patchErr
}
func (f *fakeCampaigns) ReplaceBanner(_ context.Context, _ uuid.UUID, data []byte) error {
	f.replaced = data
	return f.replaceErr
}
func (f *fakeCampaigns) UploadCodes(_ context.Context, _ uuid.UUID, csv string) (domain.UploadResult, error) {
	f.uploadCSV = csv
	return f.upload, f.uploadErr
}
func (f *fakeCampaigns) ReportRedemptions(_ context.Context, _ uuid.UUID, csv string) (domain.RedemptionResult, error) {
	f.redeemCSV = csv
	return f.redeem, f.redeemErr
}
func (f *fakeCampaigns) Stats(context.Context, uuid.UUID) (domain.CampaignStats, error) {
	return f.stats, f.statsErr
}

func newAdmin(t *testing.T, svc CampaignService) (http.Handler, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	router, err := NewAdminRouter(AdminRouterConfig{
		Campaigns:      svc,
		Logger:         slog.New(slog.NewJSONHandler(logs, nil)),
		APIKey:         testAdminKey,
		BannerMaxBytes: testBannerMax,
	})
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}
	return router, logs
}

func withKey(extra ...string) map[string]string {
	headers := map[string]string{HeaderAdminAPIKey: testAdminKey}
	for i := 0; i+1 < len(extra); i += 2 {
		headers[extra[i]] = extra[i+1]
	}
	return headers
}

// multipartBody arma un cuerpo multipart con las partes dadas. Cada parte es {nombre,
// nombre de archivo (vacío = campo de texto), contenido}.
func multipartBody(t *testing.T, parts ...[3]string) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for _, part := range parts {
		header := textproto.MIMEHeader{}
		disposition := fmt.Sprintf(`form-data; name="%s"`, part[0])
		if part[1] != "" {
			disposition += fmt.Sprintf(`; filename="%s"`, part[1])
		}
		header.Set("Content-Disposition", disposition)
		w, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(part[2])); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body, writer.FormDataContentType()
}

func pngBody(extra int) string { return pngMagicPrefix + strings.Repeat("0", extra) }

// --- Armado y autenticación ------------------------------------------------

func TestNewAdminRouterRechazaConfiguracionInsegura(t *testing.T) {
	casos := map[string]AdminRouterConfig{
		"clave corta":         {Campaigns: &fakeCampaigns{}, APIKey: "corta", BannerMaxBytes: 1},
		"clave vacía":         {Campaigns: &fakeCampaigns{}, APIKey: "", BannerMaxBytes: 1},
		"clave de 31":         {Campaigns: &fakeCampaigns{}, APIKey: strings.Repeat("k", 31), BannerMaxBytes: 1},
		"banner sin tope":     {Campaigns: &fakeCampaigns{}, APIKey: testAdminKey, BannerMaxBytes: 0},
		"banner con negativo": {Campaigns: &fakeCampaigns{}, APIKey: testAdminKey, BannerMaxBytes: -5},
	}
	for nombre, cfg := range casos {
		t.Run(nombre, func(t *testing.T) {
			if router, err := NewAdminRouter(cfg); err == nil || router != nil {
				t.Fatalf("esperaba un error al armar el router, obtuvo router=%v err=%v", router, err)
			}
		})
	}
}

func TestNewAdminRouterConClaveDe32CaracteresArma(t *testing.T) {
	_, err := NewAdminRouter(AdminRouterConfig{Campaigns: &fakeCampaigns{}, APIKey: strings.Repeat("k", 32), BannerMaxBytes: 1})

	if err != nil {
		t.Fatalf("32 caracteres es el mínimo y debe aceptarse: %v", err)
	}
}

func TestTodaRutaDeAdministracionExigeLaClave(t *testing.T) {
	router, logs := newAdmin(t, &fakeCampaigns{})
	id := uuid.NewString()
	rutas := []struct{ method, path string }{
		{http.MethodGet, campaignPath},
		{http.MethodPost, campaignPath},
		{http.MethodPatch, campaignPath + "/" + id},
		{http.MethodPut, campaignPath + "/" + id + "/banner"},
		{http.MethodPost, campaignPath + "/" + id + "/codes"},
		{http.MethodPost, campaignPath + "/" + id + "/redemptions"},
		{http.MethodGet, campaignPath + "/" + id + "/stats"},
		{http.MethodGet, "/admin/v1/ruta-que-no-existe"},
		{http.MethodGet, "/"},
		{http.MethodDelete, campaignPath},
	}
	claves := map[string]map[string]string{
		"sin clave":                         nil,
		"clave vacía":                       {HeaderAdminAPIKey: ""},
		"clave incorrecta":                  {HeaderAdminAPIKey: strings.Repeat("x", len(testAdminKey))},
		"clave más corta":                   {HeaderAdminAPIKey: testAdminKey[:10]},
		"clave más larga":                   {HeaderAdminAPIKey: testAdminKey + "extra"},
		"prefijo de la real":                {HeaderAdminAPIKey: testAdminKey[:len(testAdminKey)-1]},
		"en otro encabezado":                {"Authorization": "Bearer " + testAdminKey},
		"con el nombre de la clave interna": {"X-Internal-Service-Token": testAdminKey},
	}

	for _, ruta := range rutas {
		for nombre, headers := range claves {
			t.Run(ruta.method+" "+ruta.path+" "+nombre, func(t *testing.T) {
				rec := call(router, ruta.method, ruta.path, headers, nil)

				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, esperaba 401 (exista o no la ruta)", rec.Code)
				}
				if body := errorOf(t, rec); body.Code != "UNAUTHORIZED" {
					t.Errorf("code = %q", body.Code)
				}
			})
		}
	}
	if !strings.Contains(logs.String(), "sin clave válida") {
		t.Error("el rechazo debe quedar en el log")
	}
	if strings.Contains(logs.String(), testAdminKey[:20]) {
		t.Error("el log no debe reproducir la clave")
	}
}

func TestConLaClaveCorrectaUnaRutaInexistenteEs404YUnMetodoIncorrectoEs405(t *testing.T) {
	router, _ := newAdmin(t, &fakeCampaigns{})

	notFound := call(router, http.MethodGet, "/admin/v1/otra", withKey(), nil)
	wrongMethod := call(router, http.MethodDelete, campaignPath, withKey(), nil)

	if notFound.Code != http.StatusNotFound || wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("respuestas = %d y %d, esperaba 404 y 405", notFound.Code, wrongMethod.Code)
	}
}

func TestLasRespuestasDeAdministracionNoSeCachean(t *testing.T) {
	router, _ := newAdmin(t, &fakeCampaigns{})

	rec := call(router, http.MethodGet, campaignPath, withKey(), nil)

	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("encabezados = %v", rec.Header())
	}
}

// --- Listado y estadísticas ------------------------------------------------

func TestListDeAdministracionDevuelveLasCampañasConEstadisticas(t *testing.T) {
	id := uuid.New()
	inicio := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	svc := &fakeCampaigns{summaries: []domain.CampaignSummary{{
		Campaign: domain.Campaign{ID: id, PartnerName: "P", Title: "T", Description: "D", Terms: "C", OpenURL: "https://p.example.com",
			StartsAt: inicio, EndsAt: inicio.Add(time.Hour), Active: true, CreatedAt: inicio, UpdatedAt: inicio},
		BannerETag: "no-se-expone",
		Stats:      domain.CampaignStats{Available: 7, Assigned: 2, Burned: 1, Redeemed: 3},
	}}}
	router, _ := newAdmin(t, svc)

	rec := call(router, http.MethodGet, campaignPath, withKey(), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Items) != 1 {
		t.Fatalf("cuerpo = %s (%v)", rec.Body, err)
	}
	item := body.Items[0]
	if item["id"] != id.String() || item["partner_name"] != "P" || item["active"] != true {
		t.Errorf("item = %v", item)
	}
	stats, _ := item["stats"].(map[string]any)
	if stats["available"] != float64(7) || stats["assigned"] != float64(2) || stats["burned"] != float64(1) || stats["redeemed"] != float64(3) {
		t.Errorf("stats = %v", stats)
	}
	if strings.Contains(rec.Body.String(), "no-se-expone") {
		t.Error("el ETag del banner no es parte del contrato del listado")
	}
}

func TestListDeAdministracionSinCampañasDevuelveItemsVacio(t *testing.T) {
	router, _ := newAdmin(t, &fakeCampaigns{})

	rec := call(router, http.MethodGet, campaignPath, withKey(), nil)

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"items":[]}` {
		t.Fatalf("respuesta = %d %q", rec.Code, rec.Body)
	}
}

func TestStats(t *testing.T) {
	svc := &fakeCampaigns{stats: domain.CampaignStats{Available: 5, Assigned: 4, Burned: 3, Redeemed: 2}}
	router, _ := newAdmin(t, svc)

	rec := call(router, http.MethodGet, campaignPath+"/"+uuid.NewString()+"/stats", withKey(), nil)

	want := `{"available":5,"assigned":4,"burned":3,"redeemed":2}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("respuesta = %d %q, esperaba %s", rec.Code, rec.Body, want)
	}
}

// --- Alta ------------------------------------------------------------------

func TestCreateConMultipartDevuelve201ConElId(t *testing.T) {
	id := uuid.New()
	svc := &fakeCampaigns{createdID: id}
	router, _ := newAdmin(t, svc)
	body, contentType := multipartBody(t, [3]string{"campaign", "", validCampaign}, [3]string{"banner", "banner.png", pngBody(10)})

	rec := call(router, http.MethodPost, campaignPath, withKey("Content-Type", contentType), body)

	if rec.Code != http.StatusCreated || strings.TrimSpace(rec.Body.String()) != `{"id":"`+id.String()+`"}` {
		t.Fatalf("respuesta = %d %s", rec.Code, rec.Body)
	}
	got := svc.created
	if got.PartnerName != "Petrolera" || got.Title != "T" || got.OpenURL != "https://p.example.com" || !got.Active ||
		!got.StartsAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || !got.EndsAt.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("campos recibidos por el caso de uso = %+v", got)
	}
	if string(got.Banner) != pngBody(10) {
		t.Error("el banner debía llegar intacto")
	}
}

func TestCreateAceptaLaParteCampaignComoArchivoYEnCualquierOrden(t *testing.T) {
	svc := &fakeCampaigns{createdID: uuid.New()}
	router, _ := newAdmin(t, svc)
	body, contentType := multipartBody(t, [3]string{"banner", "b.png", pngBody(5)}, [3]string{"campaign", "campaign.json", validCampaign})

	rec := call(router, http.MethodPost, campaignPath, withKey("Content-Type", contentType), body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("respuesta = %d %s", rec.Code, rec.Body)
	}
}

func TestCreateRechazaUnMultipartMalFormado(t *testing.T) {
	campaignPart := [3]string{"campaign", "", validCampaign}
	bannerPart := [3]string{"banner", "b.png", pngBody(5)}
	casos := map[string]struct {
		parts  [][3]string
		status int
		code   string
	}{
		"sin banner":                   {[][3]string{campaignPart}, http.StatusBadRequest, "INVALID_BODY"},
		"sin campaign":                 {[][3]string{bannerPart}, http.StatusBadRequest, "INVALID_BODY"},
		"sin partes":                   {nil, http.StatusBadRequest, "INVALID_BODY"},
		"campaign repetida":            {[][3]string{campaignPart, campaignPart, bannerPart}, http.StatusBadRequest, "INVALID_BODY"},
		"banner repetido":              {[][3]string{campaignPart, bannerPart, bannerPart}, http.StatusBadRequest, "INVALID_BODY"},
		"parte desconocida":            {[][3]string{campaignPart, bannerPart, {"extra", "", "x"}}, http.StatusBadRequest, "INVALID_BODY"},
		"campaign con campo extra":     {[][3]string{{"campaign", "", `{"title":"T","admin":true}`}, bannerPart}, http.StatusBadRequest, "INVALID_BODY"},
		"campaign no es JSON":          {[][3]string{{"campaign", "", "no es json"}, bannerPart}, http.StatusBadRequest, "INVALID_BODY"},
		"campaign con datos sobrantes": {[][3]string{{"campaign", "", validCampaign + `{"x":1}`}, bannerPart}, http.StatusBadRequest, "INVALID_BODY"},
		"fecha inválida":               {[][3]string{{"campaign", "", `{"starts_at":"ayer"}`}, bannerPart}, http.StatusBadRequest, "INVALID_BODY"},
		"campaign demasiado grande":    {[][3]string{{"campaign", "", `{"title":"` + strings.Repeat("a", maxCampaignPartBytes) + `"}`}, bannerPart}, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE"},
		"banner demasiado grande":      {[][3]string{campaignPart, {"banner", "b.png", pngBody(testBannerMax)}}, http.StatusRequestEntityTooLarge, "BANNER_TOO_LARGE"},
	}
	for nombre, caso := range casos {
		t.Run(nombre, func(t *testing.T) {
			svc := &fakeCampaigns{createdID: uuid.New()}
			router, _ := newAdmin(t, svc)
			body, contentType := multipartBody(t, caso.parts...)

			rec := call(router, http.MethodPost, campaignPath, withKey("Content-Type", contentType), body)

			if rec.Code != caso.status || errorOf(t, rec).Code != caso.code {
				t.Fatalf("respuesta = %d %s; esperaba %d %s", rec.Code, rec.Body, caso.status, caso.code)
			}
			if svc.created.Title != "" || svc.created.Banner != nil {
				t.Error("una petición rechazada no debe llegar al caso de uso")
			}
		})
	}
}

func TestCreateConUnCuerpoQueNoEsMultipartEs400(t *testing.T) {
	router, _ := newAdmin(t, &fakeCampaigns{})

	rec := call(router, http.MethodPost, campaignPath, withKey("Content-Type", "application/json"), strings.NewReader(validCampaign))

	if rec.Code != http.StatusBadRequest || errorOf(t, rec).Code != "INVALID_BODY" {
		t.Fatalf("respuesta = %d %s", rec.Code, rec.Body)
	}
}

func TestCreateConUnMultipartCortadoEs400(t *testing.T) {
	router, _ := newAdmin(t, &fakeCampaigns{})
	body, contentType := multipartBody(t, [3]string{"campaign", "", validCampaign})
	cortado := bytes.NewReader(body.Bytes()[:body.Len()/2])

	rec := call(router, http.MethodPost, campaignPath, withKey("Content-Type", contentType), cortado)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %s, esperaba 400", rec.Code, rec.Body)
	}
}

// El tope global del multipart es la red de seguridad por encima de los topes de cada
// parte: lo alcanza, por ejemplo, un encabezado de parte desmedido.
func TestCreateConUnCuerpoMasGrandeQueElTopeGlobalEs413(t *testing.T) {
	router, _ := newAdmin(t, &fakeCampaigns{})
	nombreEnorme := strings.Repeat("a", testBannerMax+maxCampaignPartBytes+multipartOverheadBytes+1024)
	body, contentType := multipartBody(t, [3]string{"campaign", "", validCampaign}, [3]string{"banner", nombreEnorme, pngBody(5)})

	rec := call(router, http.MethodPost, campaignPath, withKey("Content-Type", contentType), body)

	if rec.Code != http.StatusRequestEntityTooLarge || errorOf(t, rec).Code != "PAYLOAD_TOO_LARGE" {
		t.Fatalf("respuesta = %d %s, esperaba 413 PAYLOAD_TOO_LARGE", rec.Code, rec.Body)
	}
}

func TestLosErroresDeNegocioSeTraducenAlContratoDeAdministracion(t *testing.T) {
	invalida := fmt.Errorf("no se pudo crear la campaña: %w: title es obligatorio", domain.ErrInvalidCampaign)
	casos := map[string]struct {
		err     error
		status  int
		code    string
		mensaje string
	}{
		"campaña inválida":    {invalida, http.StatusBadRequest, "INVALID_BODY", "title es obligatorio"},
		"banner inválido":     {domain.ErrInvalidBanner, http.StatusBadRequest, "INVALID_BANNER", "PNG, JPEG o WebP"},
		"banner grande":       {domain.ErrBannerTooLarge, http.StatusRequestEntityTooLarge, "BANNER_TOO_LARGE", ""},
		"campaña inexistente": {domain.ErrCampaignNotFound, http.StatusNotFound, "CAMPAIGN_NOT_FOUND", ""},
		"demasiadas líneas":   {domain.ErrTooManyLines, http.StatusBadRequest, "TOO_MANY_LINES", "100000"},
		"base caída":          {domain.ErrRepositoryUnavailable, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", ""},
		"error desconocido":   {errors.New("boom SECRETO-INTERNO"), http.StatusInternalServerError, "INTERNAL_ERROR", ""},
	}
	id := uuid.NewString()
	rutas := map[string]func(*fakeCampaigns, error) (string, string, map[string]string, func() *bytes.Buffer){
		"create": func(f *fakeCampaigns, err error) (string, string, map[string]string, func() *bytes.Buffer) {
			f.createErr = err
			return http.MethodPost, campaignPath, nil, nil
		},
		"list": func(f *fakeCampaigns, err error) (string, string, map[string]string, func() *bytes.Buffer) {
			f.listErr = err
			return http.MethodGet, campaignPath, nil, nil
		},
		"patch": func(f *fakeCampaigns, err error) (string, string, map[string]string, func() *bytes.Buffer) {
			f.patchErr = err
			return http.MethodPatch, campaignPath + "/" + id, nil, func() *bytes.Buffer { return bytes.NewBufferString(`{"active":true}`) }
		},
		"banner": func(f *fakeCampaigns, err error) (string, string, map[string]string, func() *bytes.Buffer) {
			f.replaceErr = err
			return http.MethodPut, campaignPath + "/" + id + "/banner", nil, func() *bytes.Buffer { return bytes.NewBufferString(pngBody(4)) }
		},
		"codes": func(f *fakeCampaigns, err error) (string, string, map[string]string, func() *bytes.Buffer) {
			f.uploadErr = err
			return http.MethodPost, campaignPath + "/" + id + "/codes", map[string]string{"Content-Type": "text/csv"}, func() *bytes.Buffer { return bytes.NewBufferString("ABCD-1234") }
		},
		"redemptions": func(f *fakeCampaigns, err error) (string, string, map[string]string, func() *bytes.Buffer) {
			f.redeemErr = err
			return http.MethodPost, campaignPath + "/" + id + "/redemptions", map[string]string{"Content-Type": "text/csv"}, func() *bytes.Buffer { return bytes.NewBufferString("ABCD-1234") }
		},
		"stats": func(f *fakeCampaigns, err error) (string, string, map[string]string, func() *bytes.Buffer) {
			f.statsErr = err
			return http.MethodGet, campaignPath + "/" + id + "/stats", nil, nil
		},
	}

	for rutaNombre, armar := range rutas {
		for nombre, caso := range casos {
			t.Run(rutaNombre+" "+nombre, func(t *testing.T) {
				svc := &fakeCampaigns{createdID: uuid.New()}
				method, path, extra, makeBody := armar(svc, caso.err)
				router, logs := newAdmin(t, svc)
				headers := withKey()
				for k, v := range extra {
					headers[k] = v
				}
				var rec *httptest.ResponseRecorder
				if rutaNombre == "create" {
					body, contentType := multipartBody(t, [3]string{"campaign", "", validCampaign}, [3]string{"banner", "b.png", pngBody(4)})
					headers["Content-Type"] = contentType
					rec = call(router, method, path, headers, body)
				} else if makeBody != nil {
					rec = call(router, method, path, headers, makeBody())
				} else {
					rec = call(router, method, path, headers, nil)
				}

				if rec.Code != caso.status || errorOf(t, rec).Code != caso.code {
					t.Fatalf("respuesta = %d %s; esperaba %d %s", rec.Code, rec.Body, caso.status, caso.code)
				}
				if caso.mensaje != "" && !strings.Contains(errorOf(t, rec).Error, caso.mensaje) {
					t.Errorf("mensaje = %q, esperaba que contuviera %q", errorOf(t, rec).Error, caso.mensaje)
				}
				if strings.Contains(rec.Body.String(), "SECRETO-INTERNO") || strings.Contains(rec.Body.String(), "domain:") {
					t.Errorf("la respuesta no debe exponer detalles internos: %s", rec.Body)
				}
				if caso.status == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") != "5" {
					t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
				}
				if caso.status == http.StatusInternalServerError && !strings.Contains(logs.String(), "SECRETO-INTERNO") {
					t.Error("la causa del 500 debe quedar en el log")
				}
			})
		}
	}
}

// --- Parche ----------------------------------------------------------------

func TestPatchAplicaSoloLosCamposRecibidos(t *testing.T) {
	svc := &fakeCampaigns{}
	router, _ := newAdmin(t, svc)
	id := uuid.New()

	rec := call(router, http.MethodPatch, campaignPath+"/"+id.String(), withKey(),
		strings.NewReader(`{"title":"Nuevo","active":false,"ends_at":"2027-01-01T00:00:00Z"}`))

	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("respuesta = %d %q, esperaba 204 sin cuerpo", rec.Code, rec.Body)
	}
	p := svc.patched
	if svc.patchID != id || p.Title == nil || *p.Title != "Nuevo" || p.Active == nil || *p.Active || p.EndsAt == nil {
		t.Errorf("parche recibido = %+v", p)
	}
	if p.Description != nil || p.Terms != nil || p.OpenURL != nil || p.StartsAt != nil {
		t.Errorf("los campos ausentes no deben llegar: %+v", p)
	}
}

func TestPatchRechazaLoQueNoEsUnParcheValido(t *testing.T) {
	casos := map[string]string{
		"campo no editable (partner_name)": `{"partner_name":"Otro"}`,
		"campo no editable (id)":           `{"id":"` + uuid.NewString() + `"}`,
		"campo desconocido":                `{"admin":true}`,
		"no es JSON":                       `no es json`,
		"vacío":                            ``,
		"arreglo":                          `[1,2]`,
		"tipo incorrecto":                  `{"active":"si"}`,
		"datos sobrantes":                  `{"active":true}{"x":1}`,
		"fecha inválida":                   `{"ends_at":"mañana"}`,
	}
	for nombre, cuerpo := range casos {
		t.Run(nombre, func(t *testing.T) {
			svc := &fakeCampaigns{}
			router, _ := newAdmin(t, svc)

			rec := call(router, http.MethodPatch, campaignPath+"/"+uuid.NewString(), withKey(), strings.NewReader(cuerpo))

			if rec.Code != http.StatusBadRequest || errorOf(t, rec).Code != "INVALID_BODY" {
				t.Fatalf("respuesta = %d %s", rec.Code, rec.Body)
			}
			if svc.patchID != uuid.Nil {
				t.Error("un parche inválido no debe llegar al caso de uso")
			}
		})
	}
}

func TestPatchConUnCuerpoMuyGrandeEs413(t *testing.T) {
	router, _ := newAdmin(t, &fakeCampaigns{})
	enorme := `{"title":"` + strings.Repeat("a", maxJSONBodyBytes) + `"}`

	rec := call(router, http.MethodPatch, campaignPath+"/"+uuid.NewString(), withKey(), strings.NewReader(enorme))

	if rec.Code != http.StatusRequestEntityTooLarge || errorOf(t, rec).Code != "PAYLOAD_TOO_LARGE" {
		t.Fatalf("respuesta = %d %s", rec.Code, rec.Body)
	}
}

func TestUnIdQueNoEsUUIDEsCampaignNotFoundEnTodasLasRutasConId(t *testing.T) {
	svc := &fakeCampaigns{}
	router, _ := newAdmin(t, svc)
	rutas := []struct{ method, suffix, contentType string }{
		{http.MethodPatch, "", ""},
		{http.MethodPut, "/banner", ""},
		{http.MethodPost, "/codes", "text/csv"},
		{http.MethodPost, "/redemptions", "text/csv"},
		{http.MethodGet, "/stats", ""},
	}
	for _, id := range []string{"no-es-uuid", uuid.Nil.String()} {
		for _, ruta := range rutas {
			headers := withKey()
			if ruta.contentType != "" {
				headers["Content-Type"] = ruta.contentType
			}

			rec := call(router, ruta.method, campaignPath+"/"+id+ruta.suffix, headers, strings.NewReader("x"))

			if rec.Code != http.StatusNotFound || errorOf(t, rec).Code != "CAMPAIGN_NOT_FOUND" {
				t.Errorf("%s %s = %d %s", ruta.method, ruta.suffix, rec.Code, rec.Body)
			}
		}
	}
}

// --- Banner ----------------------------------------------------------------

func TestReplaceBanner(t *testing.T) {
	svc := &fakeCampaigns{}
	router, _ := newAdmin(t, svc)

	rec := call(router, http.MethodPut, campaignPath+"/"+uuid.NewString()+"/banner", withKey("Content-Type", "image/png"), strings.NewReader(pngBody(8)))

	if rec.Code != http.StatusNoContent || string(svc.replaced) != pngBody(8) {
		t.Fatalf("respuesta = %d %q; el caso de uso debía recibir la imagen", rec.Code, rec.Body)
	}
}

func TestReplaceBannerConUnCuerpoMayorAlTopeEs413(t *testing.T) {
	svc := &fakeCampaigns{}
	router, _ := newAdmin(t, svc)

	rec := call(router, http.MethodPut, campaignPath+"/"+uuid.NewString()+"/banner", withKey(), strings.NewReader(pngBody(testBannerMax)))

	if rec.Code != http.StatusRequestEntityTooLarge || svc.replaced != nil {
		t.Fatalf("respuesta = %d %s", rec.Code, rec.Body)
	}
}

// --- CSV -------------------------------------------------------------------

func TestUploadCodes(t *testing.T) {
	svc := &fakeCampaigns{upload: domain.UploadResult{Inserted: 3, Duplicates: 1, Invalid: 2}}
	router, logs := newAdmin(t, svc)

	rec := call(router, http.MethodPost, campaignPath+"/"+uuid.NewString()+"/codes",
		withKey("Content-Type", "text/csv; charset=utf-8"), strings.NewReader("AAAA-0001\nBBBB-0002\n"))

	want := `{"inserted":3,"duplicates":1,"invalid":2}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("respuesta = %d %q, esperaba %s", rec.Code, rec.Body, want)
	}
	if svc.uploadCSV != "AAAA-0001\nBBBB-0002\n" {
		t.Errorf("el CSV recibido por el caso de uso = %q", svc.uploadCSV)
	}
	if strings.Contains(logs.String(), "AAAA-0001") {
		t.Error("el log nunca debe contener un código")
	}
}

func TestReportRedemptions(t *testing.T) {
	svc := &fakeCampaigns{redeem: domain.RedemptionResult{Matched: 1, Unknown: 1}}
	router, logs := newAdmin(t, svc)

	rec := call(router, http.MethodPost, campaignPath+"/"+uuid.NewString()+"/redemptions",
		withKey("Content-Type", "text/csv"), strings.NewReader("CANJ-0001\nNOEXISTE-9\n"))

	want := `{"matched":1,"unknown":1}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("respuesta = %d %q, esperaba %s", rec.Code, rec.Body, want)
	}
	if svc.redeemCSV != "CANJ-0001\nNOEXISTE-9\n" {
		t.Errorf("el CSV recibido = %q", svc.redeemCSV)
	}
	if strings.Contains(logs.String(), "CANJ-0001") {
		t.Error("el log nunca debe contener un código")
	}
}

func TestLasCargasCSVExigenContentTypeTextCSV(t *testing.T) {
	for _, suffix := range []string{"/codes", "/redemptions"} {
		for nombre, contentType := range map[string]string{
			"sin Content-Type": "",
			"form-urlencoded":  "application/x-www-form-urlencoded",
			"JSON":             "application/json",
			"texto plano":      "text/plain",
			"multipart":        "multipart/form-data; boundary=x",
		} {
			t.Run(suffix+" "+nombre, func(t *testing.T) {
				svc := &fakeCampaigns{}
				router, _ := newAdmin(t, svc)
				headers := withKey()
				if contentType != "" {
					headers["Content-Type"] = contentType
				}

				rec := call(router, http.MethodPost, campaignPath+"/"+uuid.NewString()+suffix, headers, strings.NewReader("AAAA-0001"))

				if rec.Code != http.StatusUnsupportedMediaType || errorOf(t, rec).Code != "UNSUPPORTED_MEDIA_TYPE" {
					t.Fatalf("respuesta = %d %s", rec.Code, rec.Body)
				}
				if svc.uploadCSV != "" || svc.redeemCSV != "" {
					t.Error("un tipo de contenido inválido no debe llegar al caso de uso")
				}
			})
		}
	}
}

func TestUnCSVMasGrandeQueElTopeEs413(t *testing.T) {
	for _, suffix := range []string{"/codes", "/redemptions"} {
		t.Run(suffix, func(t *testing.T) {
			svc := &fakeCampaigns{}
			router, _ := newAdmin(t, svc)
			enorme := strings.NewReader(strings.Repeat("A", maxCSVBodyBytes+1))

			rec := call(router, http.MethodPost, campaignPath+"/"+uuid.NewString()+suffix, withKey("Content-Type", "text/csv"), enorme)

			if rec.Code != http.StatusRequestEntityTooLarge || errorOf(t, rec).Code != "PAYLOAD_TOO_LARGE" {
				t.Fatalf("respuesta = %d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestUnCSVJustoEnElTopeSeAcepta(t *testing.T) {
	svc := &fakeCampaigns{}
	router, _ := newAdmin(t, svc)

	rec := call(router, http.MethodPost, campaignPath+"/"+uuid.NewString()+"/codes", withKey("Content-Type", "text/csv"),
		strings.NewReader(strings.Repeat("A", maxCSVBodyBytes)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
}

func TestElTopeDeUnCSVCubreElMaximoDeLineasConSaltoDeLineaWindows(t *testing.T) {
	linea := strings.Repeat("a", domain.MaxCodeLength) + "\r\n"

	if got := len(linea) * domain.MaxCSVLines; got > maxCSVBodyBytes {
		t.Fatalf("el máximo de líneas (%d bytes) no cabe en el tope %d", got, maxCSVBodyBytes)
	}
}

func TestDomainMessageQuitaElPrefijoDelPaquete(t *testing.T) {
	err := fmt.Errorf("capa: %w: campo x es obligatorio", domain.ErrInvalidCampaign)

	if got := domainMessage(err); got != "datos de campaña inválidos: campo x es obligatorio" {
		t.Errorf("mensaje = %q", got)
	}
	if got := domainMessage(errors.New("sin prefijo")); got != "sin prefijo" {
		t.Errorf("mensaje = %q", got)
	}
}
