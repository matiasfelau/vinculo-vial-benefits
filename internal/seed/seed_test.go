package seed

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

var testNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

const bannerMax = 300 * 1024

// memRepo es un repositorio en memoria que registra lo que el seed le pide.
type memRepo struct {
	campaigns map[uuid.UUID]domain.Campaign
	banners   map[uuid.UUID]domain.Banner
	codes     map[uuid.UUID]map[string]bool

	listErr, createErr, insertErr error
	createCalls                   int
}

func newMemRepo() *memRepo {
	return &memRepo{
		campaigns: map[uuid.UUID]domain.Campaign{},
		banners:   map[uuid.UUID]domain.Banner{},
		codes:     map[uuid.UUID]map[string]bool{},
	}
}

func (m *memRepo) ListCampaigns(context.Context) ([]domain.CampaignSummary, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var out []domain.CampaignSummary
	for _, c := range m.campaigns {
		out = append(out, domain.CampaignSummary{Campaign: c})
	}
	return out, nil
}

func (m *memRepo) CreateCampaign(_ context.Context, c domain.Campaign, b domain.Banner) error {
	m.createCalls++
	if m.createErr != nil {
		return m.createErr
	}
	m.campaigns[c.ID], m.banners[c.ID] = c, b
	return nil
}

func (m *memRepo) InsertCodes(_ context.Context, id uuid.UUID, codes []domain.NewCode) (int, error) {
	if m.insertErr != nil {
		return 0, m.insertErr
	}
	if m.codes[id] == nil {
		m.codes[id] = map[string]bool{}
	}
	inserted := 0
	for _, c := range codes {
		if !m.codes[id][c.HMAC] {
			m.codes[id][c.HMAC] = true
			inserted++
		}
	}
	return inserted, nil
}

func (m *memRepo) UpdateCampaign(context.Context, uuid.UUID, time.Time, func(domain.Campaign) (domain.Campaign, error)) error {
	return nil
}
func (m *memRepo) ReplaceBanner(context.Context, uuid.UUID, domain.Banner, time.Time) error {
	return nil
}
func (m *memRepo) MarkRedeemed(context.Context, uuid.UUID, []string, time.Time) (int, error) {
	return 0, nil
}
func (m *memRepo) Stats(context.Context, uuid.UUID) (domain.CampaignStats, error) {
	return domain.CampaignStats{}, nil
}

type fakeProtector struct{ err error }

func (f fakeProtector) Encrypt(code string) (string, error) { return "enc:" + code, f.err }
func (fakeProtector) HMAC(code string) string               { return "hmac:" + code }

func TestRunCargaLaCampañaVigenteConSuBannerYSusCienCodigos(t *testing.T) {
	repo := newMemRepo()

	if err := Run(context.Background(), repo, fakeProtector{}, bannerMax, testNow); err != nil {
		t.Fatalf("Run: %v", err)
	}

	active, ok := repo.campaigns[ActiveCampaignID]
	if !ok {
		t.Fatal("falta la campaña vigente")
	}
	if active.PartnerName != "Aplicación de prueba" || active.Title != "Aplicación de prueba" {
		t.Errorf("nombre = %q / %q, esperaba %q", active.PartnerName, active.Title, "Aplicación de prueba")
	}
	if !active.IsLive(testNow) || !strings.HasPrefix(active.OpenURL, "https://") {
		t.Errorf("la campaña de prueba debe estar vigente y con URL https: %+v", active)
	}
	if got := len(repo.codes[ActiveCampaignID]); got != ActiveCodeCount || ActiveCodeCount != 100 {
		t.Errorf("códigos = %d, esperaba 100", got)
	}
	banner := repo.banners[ActiveCampaignID]
	if banner.ContentType != "image/png" || len(banner.Data) == 0 || len(banner.Data) > bannerMax || len(banner.ETag) != 64 {
		t.Errorf("banner = %s, %d bytes, ETag %q", banner.ContentType, len(banner.Data), banner.ETag)
	}
}

func TestRunCargaUnaCampañaVencidaQueNoEstaVigente(t *testing.T) {
	repo := newMemRepo()

	if err := Run(context.Background(), repo, fakeProtector{}, bannerMax, testNow); err != nil {
		t.Fatalf("Run: %v", err)
	}

	expired, ok := repo.campaigns[ExpiredCampaignID]
	if !ok {
		t.Fatal("falta la campaña vencida")
	}
	if !expired.Active || expired.IsLive(testNow) || !expired.EndsAt.Before(testNow) {
		t.Errorf("la campaña vencida debe estar activa pero fuera de fecha para probar que no se muestra: %+v", expired)
	}
	if len(repo.codes[ExpiredCampaignID]) == 0 {
		t.Error("la vencida también lleva códigos: lo que la oculta es la fecha, no la falta de códigos")
	}
	if len(repo.campaigns) != 2 {
		t.Errorf("campañas = %d, esperaba 2", len(repo.campaigns))
	}
}

func TestRunEsIdempotente(t *testing.T) {
	repo := newMemRepo()

	for i := 0; i < 3; i++ {
		if err := Run(context.Background(), repo, fakeProtector{}, bannerMax, testNow); err != nil {
			t.Fatalf("ejecución %d: %v", i+1, err)
		}
	}

	if repo.createCalls != 2 {
		t.Errorf("CreateCampaign se llamó %d veces; las campañas ya cargadas no se vuelven a crear", repo.createCalls)
	}
	if got := len(repo.codes[ActiveCampaignID]); got != ActiveCodeCount {
		t.Errorf("códigos = %d tras repetir el seed, esperaba %d sin duplicar", got, ActiveCodeCount)
	}
}

func TestRunLosCodigosCargadosSonLosDeclarados(t *testing.T) {
	repo := newMemRepo()
	if err := Run(context.Background(), repo, fakeProtector{}, bannerMax, testNow); err != nil {
		t.Fatal(err)
	}

	for _, code := range []string{"PRUEBA-0001", "PRUEBA-0050", "PRUEBA-0100"} {
		if !repo.codes[ActiveCampaignID]["hmac:"+code] {
			t.Errorf("falta el código %s", code)
		}
		if !domain.ValidCode(code) {
			t.Errorf("%s no cumple el formato de un código", code)
		}
	}
	if repo.codes[ActiveCampaignID]["hmac:PRUEBA-0101"] {
		t.Error("el seed carga exactamente 100 códigos")
	}
}

func TestRunPropagaLosErrores(t *testing.T) {
	causa := errors.New("falla simulada")
	casos := map[string]*memRepo{
		"al listar": {listErr: causa},
		"al crear":  {createErr: causa},
		"al cargar": {insertErr: causa},
	}
	for nombre, repo := range casos {
		t.Run(nombre, func(t *testing.T) {
			repo.campaigns, repo.banners, repo.codes = map[uuid.UUID]domain.Campaign{}, map[uuid.UUID]domain.Banner{}, map[uuid.UUID]map[string]bool{}

			if err := Run(context.Background(), repo, fakeProtector{}, bannerMax, testNow); !errors.Is(err, causa) {
				t.Fatalf("error = %v, esperaba la causa", err)
			}
		})
	}

	t.Run("al cifrar", func(t *testing.T) {
		if err := Run(context.Background(), newMemRepo(), fakeProtector{err: causa}, bannerMax, testNow); !errors.Is(err, causa) {
			t.Fatalf("error = %v, esperaba la causa", err)
		}
	})
}

func TestRunConUnTopeDeBannerMuyChicoFalla(t *testing.T) {
	err := Run(context.Background(), newMemRepo(), fakeProtector{}, 10, testNow)

	if !errors.Is(err, domain.ErrBannerTooLarge) {
		t.Fatalf("error = %v, esperaba ErrBannerTooLarge", err)
	}
}

func TestGenerateBannerEsUnPNGValidoYChico(t *testing.T) {
	data := GenerateBanner(color.RGBA{R: 200, G: 30, B: 30, A: 255})

	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("no es un PNG decodificable: %v", err)
	}
	if b := img.Bounds(); b.Dx() != bannerWidth || b.Dy() != bannerHeight {
		t.Errorf("tamaño = %dx%d, esperaba %dx%d", b.Dx(), b.Dy(), bannerWidth, bannerHeight)
	}
	if len(data) > 300*1024 {
		t.Errorf("el banner pesa %d bytes y debe caber en BANNER_MAX_BYTES", len(data))
	}
	// Las franjas oscurecen el color base: el primer y el último píxel no pueden coincidir.
	first, last := img.At(0, 0), img.At(bannerWidth-1, 0)
	if first == last {
		t.Error("el banner debe tener franjas de distinto tono")
	}
}

func TestGenerateBannerDependeDelColor(t *testing.T) {
	rojo := GenerateBanner(color.RGBA{R: 200, A: 255})
	azul := GenerateBanner(color.RGBA{B: 200, A: 255})

	if bytes.Equal(rojo, azul) {
		t.Error("dos colores distintos deben dar banners distintos")
	}
}

func TestScaleOscurecePorLaProporcion(t *testing.T) {
	if got := scale(200, 255); got != 200 {
		t.Errorf("scale(200, 255) = %d, esperaba 200", got)
	}
	if got := scale(200, 0); got != 0 {
		t.Errorf("scale(200, 0) = %d, esperaba 0", got)
	}
	if got := scale(200, 128); got < 99 || got > 101 {
		t.Errorf("scale(200, 128) = %d, esperaba ≈100", got)
	}
}
