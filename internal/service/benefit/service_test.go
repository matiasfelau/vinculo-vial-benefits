package benefit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

var testNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// fakeRepo registra lo que recibe y devuelve resultados configurables.
type fakeRepo struct {
	benefits []domain.Benefit
	listErr  error

	meta, data       domain.Banner
	metaErr, dataErr error
	metaCalls        int
	dataCalls        int

	claimCode string
	claimErr  error

	gotNow    time.Time
	gotUser   uuid.UUID
	gotCamp   uuid.UUID
	claimSeen int
}

func (f *fakeRepo) ListLive(_ context.Context, userID uuid.UUID, now time.Time) ([]domain.Benefit, error) {
	f.gotUser, f.gotNow = userID, now
	return f.benefits, f.listErr
}

func (f *fakeRepo) BannerMeta(_ context.Context, _ uuid.UUID, now time.Time) (domain.Banner, error) {
	f.metaCalls++
	f.gotNow = now
	return f.meta, f.metaErr
}

func (f *fakeRepo) BannerData(_ context.Context, _ uuid.UUID, _ time.Time) (domain.Banner, error) {
	f.dataCalls++
	return f.data, f.dataErr
}

func (f *fakeRepo) Claim(_ context.Context, campaignID, userID uuid.UUID, now time.Time) (string, error) {
	f.claimSeen++
	f.gotCamp, f.gotUser, f.gotNow = campaignID, userID, now
	return f.claimCode, f.claimErr
}

func (f *fakeRepo) BurnByUser(context.Context, uuid.UUID) (int64, error) { return 0, nil }
func (f *fakeRepo) Ping(context.Context) error                           { return nil }

// fakeDecryptor "descifra" quitando el prefijo enc:.
type fakeDecryptor struct{ err error }

func (f fakeDecryptor) Decrypt(encoded string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return encoded[len("enc:"):], nil
}

func newService(repo *fakeRepo, dec fakeDecryptor) *Service {
	return NewService(repo, dec, func() time.Time { return testNow })
}

func TestListDescifraSoloElCodigoDeQuienLoPidio(t *testing.T) {
	user, con, sin := uuid.New(), uuid.New(), uuid.New()
	repo := &fakeRepo{benefits: []domain.Benefit{
		{ID: con, PartnerName: "A", Title: "TA", Description: "DA", Terms: "CA", OpenURL: "https://a.example.com", EndsAt: testNow.Add(time.Hour), CodeEnc: "enc:ABCD-1234"},
		{ID: sin, PartnerName: "B", Title: "TB", EndsAt: testNow.Add(2 * time.Hour)},
	}}

	views, err := newService(repo, fakeDecryptor{}).List(context.Background(), user)

	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("len = %d, esperaba 2", len(views))
	}
	if views[0].Code == nil || *views[0].Code != "ABCD-1234" {
		t.Errorf("code = %v, esperaba ABCD-1234 descifrado", views[0].Code)
	}
	if views[1].Code != nil {
		t.Errorf("code = %v, esperaba nil: todavía no lo pidió", *views[1].Code)
	}
	if views[0].ID != con || views[0].PartnerName != "A" || views[0].OpenURL != "https://a.example.com" || views[0].Terms != "CA" {
		t.Errorf("campos copiados mal: %+v", views[0])
	}
	if repo.gotUser != user || !repo.gotNow.Equal(testNow) {
		t.Errorf("el repositorio debía recibir el usuario y el reloj del servicio: %v, %v", repo.gotUser, repo.gotNow)
	}
}

func TestListSinCampañasDevuelveUnaListaVacia(t *testing.T) {
	views, err := newService(&fakeRepo{}, fakeDecryptor{}).List(context.Background(), uuid.New())

	if err != nil || views == nil || len(views) != 0 {
		t.Fatalf("List = %v, %v; esperaba una lista vacía no nula", views, err)
	}
}

func TestListPropagaLosErrores(t *testing.T) {
	t.Run("del repositorio", func(t *testing.T) {
		_, err := newService(&fakeRepo{listErr: domain.ErrRepositoryUnavailable}, fakeDecryptor{}).List(context.Background(), uuid.New())

		if !errors.Is(err, domain.ErrRepositoryUnavailable) {
			t.Fatalf("error = %v, esperaba ErrRepositoryUnavailable", err)
		}
	})

	t.Run("de un código que no se puede descifrar", func(t *testing.T) {
		repo := &fakeRepo{benefits: []domain.Benefit{{ID: uuid.New(), CodeEnc: "enc:SECRETO"}}}
		causa := errors.New("clave equivocada")

		_, err := newService(repo, fakeDecryptor{err: causa}).List(context.Background(), uuid.New())

		if !errors.Is(err, causa) {
			t.Fatalf("error = %v, esperaba la causa", err)
		}
	})
}

func TestClaimDevuelveElCodigoDescifrado(t *testing.T) {
	campaign, user := uuid.New(), uuid.New()
	repo := &fakeRepo{claimCode: "enc:PROMO-0001"}

	code, err := newService(repo, fakeDecryptor{}).Claim(context.Background(), campaign, user)

	if err != nil || code != "PROMO-0001" {
		t.Fatalf("Claim = %q, %v; esperaba PROMO-0001", code, err)
	}
	if repo.gotCamp != campaign || repo.gotUser != user || !repo.gotNow.Equal(testNow) {
		t.Errorf("el repositorio recibió %v, %v, %v", repo.gotCamp, repo.gotUser, repo.gotNow)
	}
}

func TestClaimPropagaLosErroresDeDominio(t *testing.T) {
	for _, causa := range []error{domain.ErrBenefitNotFound, domain.ErrNoCodesLeft, domain.ErrRepositoryUnavailable} {
		_, err := newService(&fakeRepo{claimErr: causa}, fakeDecryptor{}).Claim(context.Background(), uuid.New(), uuid.New())

		if !errors.Is(err, causa) {
			t.Errorf("error = %v, esperaba que conservara %v", err, causa)
		}
	}
}

func TestClaimConUnCodigoQueNoSePuedeDescifrarFalla(t *testing.T) {
	causa := errors.New("clave equivocada")

	code, err := newService(&fakeRepo{claimCode: "enc:X"}, fakeDecryptor{err: causa}).Claim(context.Background(), uuid.New(), uuid.New())

	if !errors.Is(err, causa) || code != "" {
		t.Fatalf("Claim = %q, %v; esperaba la causa y ningún código", code, err)
	}
}

func TestBannerConEtagCoincidenteNoLeeLosBytes(t *testing.T) {
	repo := &fakeRepo{meta: domain.Banner{ContentType: "image/png", ETag: "abc123"}}

	banner, notModified, err := newService(repo, fakeDecryptor{}).Banner(context.Background(), uuid.New(), `"abc123"`)

	if err != nil || !notModified {
		t.Fatalf("Banner = %v, %v, %v; esperaba notModified", banner, notModified, err)
	}
	if repo.dataCalls != 0 {
		t.Errorf("no debía leer los bytes de la imagen: %d lecturas", repo.dataCalls)
	}
	if banner.ETag != "abc123" || banner.ContentType != "image/png" {
		t.Errorf("banner = %+v, esperaba tipo y ETag", banner)
	}
}

func TestBannerSinEtagOConEtagDistintoDevuelveLaImagen(t *testing.T) {
	repo := &fakeRepo{
		meta: domain.Banner{ContentType: "image/png", ETag: "nuevo"},
		data: domain.Banner{ContentType: "image/png", ETag: "nuevo", Data: []byte("bytes")},
	}

	for _, ifNoneMatch := range []string{"", `"viejo"`, `W/"viejo"`, "basura,otra"} {
		banner, notModified, err := newService(repo, fakeDecryptor{}).Banner(context.Background(), uuid.New(), ifNoneMatch)

		if err != nil || notModified || string(banner.Data) != "bytes" {
			t.Errorf("If-None-Match %q: %+v, %v, %v; esperaba la imagen completa", ifNoneMatch, banner, notModified, err)
		}
	}
}

func TestBannerPropagaLosErrores(t *testing.T) {
	t.Run("al leer el tipo", func(t *testing.T) {
		_, _, err := newService(&fakeRepo{metaErr: domain.ErrBenefitNotFound}, fakeDecryptor{}).Banner(context.Background(), uuid.New(), "")

		if !errors.Is(err, domain.ErrBenefitNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("al leer los bytes", func(t *testing.T) {
		repo := &fakeRepo{meta: domain.Banner{ETag: "x"}, dataErr: domain.ErrRepositoryUnavailable}

		_, _, err := newService(repo, fakeDecryptor{}).Banner(context.Background(), uuid.New(), "")

		if !errors.Is(err, domain.ErrRepositoryUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestETagMatches(t *testing.T) {
	casos := []struct {
		header string
		etag   string
		match  bool
	}{
		{`"abc"`, "abc", true},
		{`abc`, "abc", true},
		{`W/"abc"`, "abc", true},
		{`"x", "abc"`, "abc", true},
		{`"x" , W/"abc" `, "abc", true},
		{`*`, "abc", true},
		{`"abd"`, "abc", false},
		{``, "abc", false},
		{`""`, "", false},
		{``, "", false},
		{`"ab"`, "abc", false},
	}
	for _, caso := range casos {
		if got := ETagMatches(caso.header, caso.etag); got != caso.match {
			t.Errorf("ETagMatches(%q, %q) = %v, esperaba %v", caso.header, caso.etag, got, caso.match)
		}
	}
}

func TestNewServiceSinRelojUsaElDelSistema(t *testing.T) {
	repo := &fakeRepo{}
	antes := time.Now()

	if _, err := NewService(repo, fakeDecryptor{}, nil).List(context.Background(), uuid.New()); err != nil {
		t.Fatal(err)
	}

	if repo.gotNow.Before(antes) || repo.gotNow.After(time.Now()) {
		t.Errorf("now = %v, esperaba la hora del sistema", repo.gotNow)
	}
}
