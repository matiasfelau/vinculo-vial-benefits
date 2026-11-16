package campaign

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

var testNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

const maxBanner = 1024

// fakeRepo guarda lo que recibe y devuelve resultados configurables.
type fakeRepo struct {
	created       domain.Campaign
	createdBanner domain.Banner
	createErr     error
	createCalls   int

	summaries []domain.CampaignSummary
	listErr   error

	current   domain.Campaign
	updateErr error
	updated   domain.Campaign
	updatedAt time.Time

	replacedBanner domain.Banner
	replaceErr     error
	replaceCalls   int

	insertedCodes []domain.NewCode
	insertResult  int
	insertErr     error
	insertCalls   int

	redeemHMACs  []string
	redeemResult int
	redeemErr    error
	redeemAt     time.Time

	stats    domain.CampaignStats
	statsErr error
}

func (f *fakeRepo) CreateCampaign(_ context.Context, c domain.Campaign, b domain.Banner) error {
	f.createCalls++
	f.created, f.createdBanner = c, b
	return f.createErr
}

func (f *fakeRepo) ListCampaigns(context.Context) ([]domain.CampaignSummary, error) {
	return f.summaries, f.listErr
}

func (f *fakeRepo) UpdateCampaign(_ context.Context, _ uuid.UUID, now time.Time, mutate func(domain.Campaign) (domain.Campaign, error)) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	updated, err := mutate(f.current)
	if err != nil {
		return err
	}
	f.updated, f.updatedAt = updated, now
	return nil
}

func (f *fakeRepo) ReplaceBanner(_ context.Context, _ uuid.UUID, b domain.Banner, _ time.Time) error {
	f.replaceCalls++
	f.replacedBanner = b
	return f.replaceErr
}

func (f *fakeRepo) InsertCodes(_ context.Context, _ uuid.UUID, codes []domain.NewCode) (int, error) {
	f.insertCalls++
	f.insertedCodes = codes
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	// insertResult negativo significa "la base insertó todos".
	if f.insertResult >= 0 {
		return f.insertResult, nil
	}
	return len(codes), nil
}

func (f *fakeRepo) MarkRedeemed(_ context.Context, _ uuid.UUID, hmacs []string, now time.Time) (int, error) {
	f.redeemHMACs, f.redeemAt = hmacs, now
	return f.redeemResult, f.redeemErr
}

func (f *fakeRepo) Stats(context.Context, uuid.UUID) (domain.CampaignStats, error) {
	return f.stats, f.statsErr
}

// fakeProtector "cifra" con un prefijo y "calcula el HMAC" con otro.
type fakeProtector struct{ encErr error }

func (f fakeProtector) Encrypt(code string) (string, error) {
	if f.encErr != nil {
		return "", f.encErr
	}
	return "enc:" + code, nil
}

func (fakeProtector) HMAC(code string) string { return "hmac:" + code }

func newService(repo *fakeRepo) *Service {
	return NewService(repo, fakeProtector{}, maxBanner, func() time.Time { return testNow })
}

func pngBanner() []byte {
	return []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("0", 32))
}

func validInput() CreateInput {
	return CreateInput{
		PartnerName: "  Petrolera Sur  ",
		Title:       "10 % de descuento",
		Description: "Descuento en combustible",
		Terms:       "Un código por persona",
		OpenURL:     "https://partner.example.com/canje",
		StartsAt:    testNow.Add(-time.Hour),
		EndsAt:      testNow.Add(24 * time.Hour),
		Active:      true,
		Banner:      pngBanner(),
	}
}

// --- Create ----------------------------------------------------------------

func TestCreatePersisteLaCampañaConSuBanner(t *testing.T) {
	repo := &fakeRepo{}

	id, err := newService(repo).Create(context.Background(), validInput())

	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == uuid.Nil || repo.created.ID != id {
		t.Errorf("id devuelto %v, id persistido %v", id, repo.created.ID)
	}
	if repo.created.PartnerName != "Petrolera Sur" {
		t.Errorf("partner_name = %q; los espacios de los bordes se recortan", repo.created.PartnerName)
	}
	if !repo.created.Active || !repo.created.CreatedAt.Equal(testNow) || !repo.created.UpdatedAt.Equal(testNow) {
		t.Errorf("campaña = %+v", repo.created)
	}
	if repo.createdBanner.ContentType != "image/png" || len(repo.createdBanner.ETag) != 64 {
		t.Errorf("banner = %+v, esperaba el tipo detectado y el SHA-256", repo.createdBanner)
	}
}

func TestCreateQuedaInactivaSiNoSePideLoContrario(t *testing.T) {
	repo := &fakeRepo{}
	input := validInput()
	input.Active = false

	if _, err := newService(repo).Create(context.Background(), input); err != nil {
		t.Fatal(err)
	}

	if repo.created.Active {
		t.Error("la campaña debía quedar inactiva")
	}
}

func TestCreateConvierteLasFechasAUTC(t *testing.T) {
	repo := &fakeRepo{}
	input := validInput()
	zona := time.FixedZone("ART", -3*3600)
	input.StartsAt, input.EndsAt = input.StartsAt.In(zona), input.EndsAt.In(zona)

	if _, err := newService(repo).Create(context.Background(), input); err != nil {
		t.Fatal(err)
	}

	if repo.created.StartsAt.Location() != time.UTC || repo.created.EndsAt.Location() != time.UTC {
		t.Errorf("las fechas debían guardarse en UTC: %v, %v", repo.created.StartsAt.Location(), repo.created.EndsAt.Location())
	}
}

func TestCreateRechazaLaEntradaInvalidaSinPersistir(t *testing.T) {
	casos := map[string]struct {
		mutar func(*CreateInput)
		causa error
	}{
		"título vacío":            {func(i *CreateInput) { i.Title = " " }, domain.ErrInvalidCampaign},
		"URL http":                {func(i *CreateInput) { i.OpenURL = "http://partner.example.com" }, domain.ErrInvalidCampaign},
		"fechas invertidas":       {func(i *CreateInput) { i.EndsAt = i.StartsAt.Add(-time.Hour) }, domain.ErrInvalidCampaign},
		"banner que no es imagen": {func(i *CreateInput) { i.Banner = []byte("no soy una imagen") }, domain.ErrInvalidBanner},
		"banner vacío":            {func(i *CreateInput) { i.Banner = nil }, domain.ErrInvalidBanner},
		"banner demasiado grande": {func(i *CreateInput) { i.Banner = append(pngBanner(), make([]byte, maxBanner)...) }, domain.ErrBannerTooLarge},
	}
	for nombre, caso := range casos {
		t.Run(nombre, func(t *testing.T) {
			repo := &fakeRepo{}
			input := validInput()
			caso.mutar(&input)

			id, err := newService(repo).Create(context.Background(), input)

			if !errors.Is(err, caso.causa) || id != uuid.Nil {
				t.Fatalf("Create = %v, %v; esperaba %v", id, err, caso.causa)
			}
			if repo.createCalls != 0 {
				t.Error("una entrada inválida no debe llegar a la base")
			}
		})
	}
}

func TestCreatePropagaElErrorDelRepositorio(t *testing.T) {
	repo := &fakeRepo{createErr: domain.ErrRepositoryUnavailable}

	_, err := newService(repo).Create(context.Background(), validInput())

	if !errors.Is(err, domain.ErrRepositoryUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

// --- List / Stats ----------------------------------------------------------

func TestListYStatsDelegan(t *testing.T) {
	repo := &fakeRepo{
		summaries: []domain.CampaignSummary{{Campaign: domain.Campaign{ID: uuid.New()}}},
		stats:     domain.CampaignStats{Available: 3, Assigned: 2, Burned: 1, Redeemed: 4},
	}
	svc := newService(repo)

	summaries, err := svc.List(context.Background())
	stats, errStats := svc.Stats(context.Background(), uuid.New())

	if err != nil || len(summaries) != 1 {
		t.Errorf("List = %v, %v", summaries, err)
	}
	if errStats != nil || stats != repo.stats {
		t.Errorf("Stats = %+v, %v", stats, errStats)
	}
}

func TestListYStatsPropaganLosErrores(t *testing.T) {
	repo := &fakeRepo{listErr: domain.ErrRepositoryUnavailable, statsErr: domain.ErrCampaignNotFound}
	svc := newService(repo)

	if _, err := svc.List(context.Background()); !errors.Is(err, domain.ErrRepositoryUnavailable) {
		t.Errorf("List: error = %v", err)
	}
	if _, err := svc.Stats(context.Background(), uuid.New()); !errors.Is(err, domain.ErrCampaignNotFound) {
		t.Errorf("Stats: error = %v", err)
	}
}

// --- Patch -----------------------------------------------------------------

func currentCampaign() domain.Campaign {
	return domain.Campaign{
		ID: uuid.New(), PartnerName: "Petrolera", Title: "Título", Description: "Descripción", Terms: "Condiciones",
		OpenURL: "https://partner.example.com", StartsAt: testNow.Add(-time.Hour), EndsAt: testNow.Add(time.Hour), Active: true,
	}
}

func TestPatchAplicaSoloLosCamposPresentesYMueveUpdatedAt(t *testing.T) {
	repo := &fakeRepo{current: currentCampaign()}
	titulo, activa := "  Otro título  ", false

	err := newService(repo).Patch(context.Background(), repo.current.ID, domain.CampaignPatch{Title: &titulo, Active: &activa})

	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if repo.updated.Title != "Otro título" || repo.updated.Active {
		t.Errorf("campaña = %+v; esperaba título recortado y estado inactivo", repo.updated)
	}
	if repo.updated.Description != "Descripción" || repo.updated.PartnerName != "Petrolera" {
		t.Errorf("el parche tocó campos que no traía: %+v", repo.updated)
	}
	if !repo.updatedAt.Equal(testNow) {
		t.Errorf("updated_at = %v, esperaba el reloj del servicio", repo.updatedAt)
	}
}

func TestPatchValidaLaCampañaResultante(t *testing.T) {
	vacio, http := "", "http://partner.example.com"
	antes := testNow.Add(-2 * time.Hour)
	largo := strings.Repeat("a", 81)

	casos := map[string]domain.CampaignPatch{
		"título vacío":              {Title: &vacio},
		"título largo":              {Title: &largo},
		"URL http":                  {OpenURL: &http},
		"ends_at antes de starts":   {EndsAt: &antes},
		"starts_at después de ends": {StartsAt: ptr(testNow.Add(2 * time.Hour))},
	}
	for nombre, patch := range casos {
		t.Run(nombre, func(t *testing.T) {
			repo := &fakeRepo{current: currentCampaign()}

			err := newService(repo).Patch(context.Background(), repo.current.ID, patch)

			if !errors.Is(err, domain.ErrInvalidCampaign) {
				t.Fatalf("error = %v, esperaba ErrInvalidCampaign", err)
			}
			if repo.updated.ID != uuid.Nil {
				t.Error("un parche inválido no debe guardarse")
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestPatchVacioEsInvalido(t *testing.T) {
	repo := &fakeRepo{current: currentCampaign()}

	err := newService(repo).Patch(context.Background(), repo.current.ID, domain.CampaignPatch{})

	if !errors.Is(err, domain.ErrInvalidCampaign) {
		t.Fatalf("error = %v, esperaba ErrInvalidCampaign", err)
	}
}

func TestPatchConvierteLasFechasAUTC(t *testing.T) {
	repo := &fakeRepo{current: currentCampaign()}
	zona := time.FixedZone("ART", -3*3600)
	fin := testNow.Add(48 * time.Hour).In(zona)

	if err := newService(repo).Patch(context.Background(), repo.current.ID, domain.CampaignPatch{EndsAt: &fin}); err != nil {
		t.Fatal(err)
	}

	if repo.updated.EndsAt.Location() != time.UTC || !repo.updated.EndsAt.Equal(fin) {
		t.Errorf("ends_at = %v, esperaba el mismo instante en UTC", repo.updated.EndsAt)
	}
}

func TestPatchPropagaElErrorDelRepositorio(t *testing.T) {
	repo := &fakeRepo{updateErr: domain.ErrCampaignNotFound}

	err := newService(repo).Patch(context.Background(), uuid.New(), domain.CampaignPatch{Active: ptr(true)})

	if !errors.Is(err, domain.ErrCampaignNotFound) {
		t.Fatalf("error = %v", err)
	}
}

// --- ReplaceBanner ---------------------------------------------------------

func TestReplaceBanner(t *testing.T) {
	repo := &fakeRepo{}

	if err := newService(repo).ReplaceBanner(context.Background(), uuid.New(), pngBanner()); err != nil {
		t.Fatalf("ReplaceBanner: %v", err)
	}

	if repo.replacedBanner.ContentType != "image/png" || len(repo.replacedBanner.ETag) != 64 {
		t.Errorf("banner = %+v", repo.replacedBanner)
	}
}

func TestReplaceBannerRechazaLoInvalidoSinTocarLaBase(t *testing.T) {
	casos := map[string]struct {
		data  []byte
		causa error
	}{
		"no es una imagen": {[]byte("texto"), domain.ErrInvalidBanner},
		"demasiado grande": {append(pngBanner(), make([]byte, maxBanner)...), domain.ErrBannerTooLarge},
	}
	for nombre, caso := range casos {
		t.Run(nombre, func(t *testing.T) {
			repo := &fakeRepo{}

			err := newService(repo).ReplaceBanner(context.Background(), uuid.New(), caso.data)

			if !errors.Is(err, caso.causa) || repo.replaceCalls != 0 {
				t.Fatalf("error = %v, llamadas = %d", err, repo.replaceCalls)
			}
		})
	}
}

func TestReplaceBannerPropagaElErrorDelRepositorio(t *testing.T) {
	repo := &fakeRepo{replaceErr: domain.ErrCampaignNotFound}

	err := newService(repo).ReplaceBanner(context.Background(), uuid.New(), pngBanner())

	if !errors.Is(err, domain.ErrCampaignNotFound) {
		t.Fatalf("error = %v", err)
	}
}

// --- UploadCodes -----------------------------------------------------------

func TestUploadCodesCargaLosValidosYContabilizaLoDemas(t *testing.T) {
	repo := &fakeRepo{insertResult: 3}
	csv := "AAAA-0001\r\nAAAA-0002\n\n  AAAA-0003  \nAAAA-0001\nmal código\nabc\nAAAA-0004\n"

	result, err := newService(repo).UploadCodes(context.Background(), uuid.New(), csv)

	if err != nil {
		t.Fatalf("UploadCodes: %v", err)
	}
	// Válidos únicos: 0001, 0002, 0003, 0004 (4). Se repite 0001 (1 duplicado en el
	// archivo). Inválidos: "mal código" y "abc". La base insertó 3 de 4: el cuarto ya existía.
	want := domain.UploadResult{Inserted: 3, Duplicates: 2, Invalid: 2}
	if result != want {
		t.Errorf("resultado = %+v, esperaba %+v", result, want)
	}
	if len(repo.insertedCodes) != 4 {
		t.Fatalf("códigos enviados a la base = %d, esperaba 4 únicos y válidos", len(repo.insertedCodes))
	}
	if repo.insertedCodes[0] != (domain.NewCode{Enc: "enc:AAAA-0001", HMAC: "hmac:AAAA-0001"}) {
		t.Errorf("primer código = %+v; debía enviarse cifrado y con su HMAC, nunca en claro", repo.insertedCodes[0])
	}
	for _, c := range repo.insertedCodes {
		if strings.Contains(c.HMAC, "enc:") || !strings.HasPrefix(c.Enc, "enc:") {
			t.Errorf("código mal armado: %+v", c)
		}
	}
}

func TestUploadCodesIgnoraElBOMYLasLineasEnBlanco(t *testing.T) {
	repo := &fakeRepo{insertResult: -1}

	result, err := newService(repo).UploadCodes(context.Background(), uuid.New(), "\xEF\xBB\xBFBOMM-0001\n\n\n   \nBOMM-0002")

	if err != nil || result != (domain.UploadResult{Inserted: 2}) {
		t.Fatalf("UploadCodes = %+v, %v; esperaba 2 insertados y nada inválido", result, err)
	}
}

func TestUploadCodesDeUnArchivoVacioNoInsertaNada(t *testing.T) {
	repo := &fakeRepo{insertResult: -1}

	result, err := newService(repo).UploadCodes(context.Background(), uuid.New(), "")

	if err != nil || result != (domain.UploadResult{}) {
		t.Fatalf("UploadCodes = %+v, %v", result, err)
	}
}

func TestUploadCodesRechazaMasDelMaximoDeLineas(t *testing.T) {
	repo := &fakeRepo{insertResult: -1}
	var csv strings.Builder
	for i := 0; i <= domain.MaxCSVLines; i++ {
		fmt.Fprintf(&csv, "COD-%06d\n", i)
	}

	_, err := newService(repo).UploadCodes(context.Background(), uuid.New(), csv.String())

	if !errors.Is(err, domain.ErrTooManyLines) {
		t.Fatalf("error = %v, esperaba ErrTooManyLines", err)
	}
	if repo.insertCalls != 0 {
		t.Error("un archivo que se pasa del máximo no debe cargarse ni en parte")
	}
}

func TestUploadCodesAceptaExactamenteElMaximoDeLineas(t *testing.T) {
	repo := &fakeRepo{insertResult: -1}
	var csv strings.Builder
	for i := 0; i < domain.MaxCSVLines; i++ {
		fmt.Fprintf(&csv, "COD-%06d\n", i)
	}

	result, err := newService(repo).UploadCodes(context.Background(), uuid.New(), csv.String())

	if err != nil || result.Inserted != domain.MaxCSVLines {
		t.Fatalf("UploadCodes = %+v, %v; esperaba %d insertados", result, err, domain.MaxCSVLines)
	}
}

func TestUploadCodesPropagaLosErrores(t *testing.T) {
	t.Run("del cifrado", func(t *testing.T) {
		causa := errors.New("sin aleatoriedad")
		svc := NewService(&fakeRepo{}, fakeProtector{encErr: causa}, maxBanner, nil)

		_, err := svc.UploadCodes(context.Background(), uuid.New(), "AAAA-0001")

		if !errors.Is(err, causa) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("del repositorio", func(t *testing.T) {
		repo := &fakeRepo{insertErr: domain.ErrCampaignNotFound}

		_, err := newService(repo).UploadCodes(context.Background(), uuid.New(), "AAAA-0001")

		if !errors.Is(err, domain.ErrCampaignNotFound) {
			t.Fatalf("error = %v", err)
		}
	})
}

// --- ReportRedemptions -----------------------------------------------------

func TestReportRedemptionsEmparejaPorHMACYContabilizaLosDesconocidos(t *testing.T) {
	repo := &fakeRepo{redeemResult: 1}
	csv := "CANJ-0001\nCANJ-9999\nCANJ-9999\n\nno es un código\n"

	result, err := newService(repo).ReportRedemptions(context.Background(), uuid.New(), csv)

	if err != nil {
		t.Fatalf("ReportRedemptions: %v", err)
	}
	// Códigos únicos válidos: 0001 y 9999. Emparejó 1; el otro y la línea inválida son desconocidos.
	if want := (domain.RedemptionResult{Matched: 1, Unknown: 2}); result != want {
		t.Errorf("resultado = %+v, esperaba %+v", result, want)
	}
	if fmt.Sprint(repo.redeemHMACs) != "[hmac:CANJ-0001 hmac:CANJ-9999]" {
		t.Errorf("HMAC enviados = %v; la base solo recibe HMAC, nunca el código en claro", repo.redeemHMACs)
	}
	if !repo.redeemAt.Equal(testNow) {
		t.Errorf("redeemed_at = %v, esperaba el reloj del servicio", repo.redeemAt)
	}
}

func TestReportRedemptionsRechazaMasDelMaximoDeLineas(t *testing.T) {
	var csv strings.Builder
	for i := 0; i <= domain.MaxCSVLines; i++ {
		fmt.Fprintf(&csv, "COD-%06d\n", i)
	}

	_, err := newService(&fakeRepo{}).ReportRedemptions(context.Background(), uuid.New(), csv.String())

	if !errors.Is(err, domain.ErrTooManyLines) {
		t.Fatalf("error = %v, esperaba ErrTooManyLines", err)
	}
}

func TestReportRedemptionsPropagaElErrorDelRepositorio(t *testing.T) {
	repo := &fakeRepo{redeemErr: domain.ErrCampaignNotFound}

	_, err := newService(repo).ReportRedemptions(context.Background(), uuid.New(), "CANJ-0001")

	if !errors.Is(err, domain.ErrCampaignNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewServiceSinRelojUsaElDelSistema(t *testing.T) {
	repo := &fakeRepo{}
	antes := time.Now()

	if _, err := NewService(repo, fakeProtector{}, maxBanner, nil).Create(context.Background(), func() CreateInput {
		i := validInput()
		i.StartsAt, i.EndsAt = antes.Add(-time.Hour), antes.Add(time.Hour)
		return i
	}()); err != nil {
		t.Fatal(err)
	}

	if repo.created.CreatedAt.Before(antes.UTC()) || repo.created.CreatedAt.After(time.Now().UTC()) {
		t.Errorf("created_at = %v, esperaba la hora del sistema", repo.created.CreatedAt)
	}
}
