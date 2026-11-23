package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
	"github.com/matiasfelau/vinculo-vial-benefits/internal/testutil/pgtest"
)

var fixedNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// newRepo abre un repositorio sobre un esquema propio de la prueba.
func newRepo(t *testing.T, opts ...Option) (*Repository, *gorm.DB) {
	t.Helper()

	db, err := Connect(pgtest.NewSchemaDSN(t))
	if err != nil {
		t.Fatalf("no se pudo conectar a la base de pruebas: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := AutoMigrate(db); err != nil {
		t.Fatalf("no se pudo migrar el esquema: %v", err)
	}
	return NewRepository(db, opts...), db
}

// fakeBanner es un banner con bytes reconocibles.
func fakeBanner(seed string) domain.Banner {
	sum := sha256.Sum256([]byte(seed))
	return domain.Banner{ContentType: "image/png", ETag: hex.EncodeToString(sum[:]), Data: []byte("png-" + seed)}
}

// newCampaign crea una campaña vigente en fixedNow, personalizable.
func newCampaign(t *testing.T, repo *Repository, mutate ...func(*domain.Campaign)) domain.Campaign {
	t.Helper()

	campaign := domain.Campaign{
		ID:          uuid.New(),
		PartnerName: "Petrolera",
		Title:       "Descuento",
		Description: "Descripción",
		Terms:       "Condiciones",
		OpenURL:     "https://partner.example.com",
		StartsAt:    fixedNow.Add(-24 * time.Hour),
		EndsAt:      fixedNow.Add(24 * time.Hour),
		Active:      true,
		CreatedAt:   fixedNow,
		UpdatedAt:   fixedNow,
	}
	for _, m := range mutate {
		m(&campaign)
	}
	if err := repo.CreateCampaign(context.Background(), campaign, fakeBanner(campaign.ID.String())); err != nil {
		t.Fatalf("no se pudo crear la campaña: %v", err)
	}
	return campaign
}

func fakeHMAC(code string) string {
	sum := sha256.Sum256([]byte("hmac:" + code))
	return hex.EncodeToString(sum[:])
}

// addCodes carga códigos cuyo "cifrado" es enc:<código>.
func addCodes(t *testing.T, repo *Repository, campaignID uuid.UUID, codes ...string) {
	t.Helper()

	batch := make([]domain.NewCode, 0, len(codes))
	for _, code := range codes {
		batch = append(batch, domain.NewCode{Enc: "enc:" + code, HMAC: fakeHMAC(code)})
	}
	inserted, err := repo.InsertCodes(context.Background(), campaignID, batch)
	if err != nil || inserted != len(codes) {
		t.Fatalf("InsertCodes = %d, %v; esperaba %d sin error", inserted, err, len(codes))
	}
}

func numberedCodes(prefix string, n int) []string {
	codes := make([]string, n)
	for i := range codes {
		codes[i] = fmt.Sprintf("%s-%04d", prefix, i+1)
	}
	return codes
}

func statsOf(t *testing.T, repo *Repository, campaignID uuid.UUID) domain.CampaignStats {
	t.Helper()
	stats, err := repo.Stats(context.Background(), campaignID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	return stats
}

// --- Esquema ---------------------------------------------------------------

func TestAutoMigrateEsIdempotenteYCreaLosIndicesDelContrato(t *testing.T) {
	_, db := newRepo(t)

	if err := AutoMigrate(db); err != nil {
		t.Fatalf("una segunda migración no debía fallar: %v", err)
	}

	var defs []string
	if err := db.Raw(`SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'codes'`).Scan(&defs).Error; err != nil {
		t.Fatal(err)
	}
	var tieneHMACUnico, tieneAsignacionParcial bool
	for _, def := range defs {
		if containsAll(def, "UNIQUE", "(campaign_id, code_hmac)") {
			tieneHMACUnico = true
		}
		if containsAll(def, "UNIQUE", "(campaign_id, assigned_to)", "WHERE (assigned_to IS NOT NULL)") {
			tieneAsignacionParcial = true
		}
	}
	if !tieneHMACUnico || !tieneAsignacionParcial {
		t.Errorf("faltan índices del contrato (hmac único = %v, asignación parcial = %v): %v", tieneHMACUnico, tieneAsignacionParcial, defs)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func TestElEsquemaImponeLasReglasDelContrato(t *testing.T) {
	repo, db := newRepo(t)
	campaign := newCampaign(t, repo)
	other := uuid.New()

	t.Run("ends_at debe ser posterior a starts_at", func(t *testing.T) {
		err := db.Exec(`UPDATE campaigns SET ends_at = starts_at WHERE id = ?`, campaign.ID).Error
		if err == nil {
			t.Fatal("la base debía rechazar ends_at <= starts_at")
		}
	})

	t.Run("el estado de un código solo admite los tres valores", func(t *testing.T) {
		err := db.Exec(`INSERT INTO codes (id, campaign_id, code_enc, code_hmac, status) VALUES (?, ?, 'x', ?, 'OTRO')`,
			uuid.New(), campaign.ID, fakeHMAC("estado")).Error
		if err == nil {
			t.Fatal("la base debía rechazar un estado desconocido")
		}
	})

	t.Run("un código no puede apuntar a una campaña inexistente", func(t *testing.T) {
		err := db.Exec(`INSERT INTO codes (id, campaign_id, code_enc, code_hmac, status) VALUES (?, ?, 'x', ?, 'AVAILABLE')`,
			uuid.New(), other, fakeHMAC("huérfano")).Error
		if err == nil {
			t.Fatal("la base debía rechazar una clave foránea inexistente")
		}
	})

	t.Run("una persona no puede tener dos códigos asignados en la misma campaña", func(t *testing.T) {
		addCodes(t, repo, campaign.ID, "UNO-1111", "DOS-2222")
		user := uuid.New()
		first := db.Exec(`UPDATE codes SET status = 'ASSIGNED', assigned_to = ? WHERE code_hmac = ?`, user, fakeHMAC("UNO-1111"))
		second := db.Exec(`UPDATE codes SET status = 'ASSIGNED', assigned_to = ? WHERE code_hmac = ?`, user, fakeHMAC("DOS-2222"))
		if first.Error != nil || second.Error == nil {
			t.Fatalf("el segundo código del mismo usuario debía rechazarse (primero: %v, segundo: %v)", first.Error, second.Error)
		}
	})

	t.Run("el mismo código no puede cargarse dos veces en la campaña", func(t *testing.T) {
		err := db.Exec(`INSERT INTO codes (id, campaign_id, code_enc, code_hmac, status) VALUES (?, ?, 'x', ?, 'AVAILABLE')`,
			uuid.New(), campaign.ID, fakeHMAC("UNO-1111")).Error
		if err == nil {
			t.Fatal("la base debía rechazar un code_hmac repetido en la campaña")
		}
	})
}

// --- Claim -----------------------------------------------------------------

func TestClaimEntregaUnCodigoYEsIdempotente(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, numberedCodes("IDEM", 5)...)
	user := uuid.New()

	first, err := repo.Claim(context.Background(), campaign.ID, user, fixedNow)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	for i := 0; i < 3; i++ {
		again, err := repo.Claim(context.Background(), campaign.ID, user, fixedNow)
		if err != nil || again != first {
			t.Fatalf("pedido %d: %q, %v; esperaba el mismo código %q", i+2, again, err, first)
		}
	}

	stats := statsOf(t, repo, campaign.ID)
	if stats.Assigned != 1 || stats.Available != 4 {
		t.Errorf("stats = %+v; pedir varias veces consume un solo código", stats)
	}
}

func TestClaimDaCodigosDistintosAPersonasDistintas(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, numberedCodes("DIST", 3)...)

	vistos := map[string]bool{}
	for i := 0; i < 3; i++ {
		code, err := repo.Claim(context.Background(), campaign.ID, uuid.New(), fixedNow)
		if err != nil {
			t.Fatalf("Claim %d: %v", i, err)
		}
		if vistos[code] {
			t.Fatalf("el código %q se entregó a dos personas", code)
		}
		vistos[code] = true
	}
}

func TestClaimConElPoolAgotadoDevuelveNoCodesLeftPeroElQueYaTeniaLoSigueViendo(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, "UNICO-0001")
	titular := uuid.New()

	code, err := repo.Claim(context.Background(), campaign.ID, titular, fixedNow)
	if err != nil {
		t.Fatalf("el primero debía recibir el único código: %v", err)
	}

	if _, err := repo.Claim(context.Background(), campaign.ID, uuid.New(), fixedNow); !errors.Is(err, domain.ErrNoCodesLeft) {
		t.Fatalf("un usuario nuevo con el pool agotado: error = %v, esperaba ErrNoCodesLeft", err)
	}
	again, err := repo.Claim(context.Background(), campaign.ID, titular, fixedNow)
	if err != nil || again != code {
		t.Fatalf("el que ya tenía código lo sigue viendo: %q, %v; esperaba %q", again, err, code)
	}
}

func TestClaimSinCodigosCargadosDevuelveNoCodesLeft(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)

	if _, err := repo.Claim(context.Background(), campaign.ID, uuid.New(), fixedNow); !errors.Is(err, domain.ErrNoCodesLeft) {
		t.Fatalf("error = %v, esperaba ErrNoCodesLeft", err)
	}
}

func TestClaimDeUnaCampañaNoVigenteDevuelveBenefitNotFound(t *testing.T) {
	repo, _ := newRepo(t)
	casos := map[string]func(*domain.Campaign){
		"inactiva":          func(c *domain.Campaign) { c.Active = false },
		"todavía no empezó": func(c *domain.Campaign) { c.StartsAt = fixedNow.Add(time.Hour); c.EndsAt = fixedNow.Add(2 * time.Hour) },
		"ya venció": func(c *domain.Campaign) {
			c.StartsAt = fixedNow.Add(-2 * time.Hour)
			c.EndsAt = fixedNow.Add(-time.Hour)
		},
		"termina ahora": func(c *domain.Campaign) { c.EndsAt = fixedNow },
	}
	for nombre, mutar := range casos {
		t.Run(nombre, func(t *testing.T) {
			campaign := newCampaign(t, repo, mutar)
			addCodes(t, repo, campaign.ID, "NOVIG-"+uuid.NewString()[:6])

			_, err := repo.Claim(context.Background(), campaign.ID, uuid.New(), fixedNow)

			if !errors.Is(err, domain.ErrBenefitNotFound) {
				t.Fatalf("error = %v, esperaba ErrBenefitNotFound", err)
			}
			if stats := statsOf(t, repo, campaign.ID); stats.Assigned != 0 {
				t.Errorf("no debía asignarse nada: %+v", stats)
			}
		})
	}
}

func TestClaimDeUnaCampañaInexistenteDevuelveBenefitNotFound(t *testing.T) {
	repo, _ := newRepo(t)

	if _, err := repo.Claim(context.Background(), uuid.New(), uuid.New(), fixedNow); !errors.Is(err, domain.ErrBenefitNotFound) {
		t.Fatalf("error = %v, esperaba ErrBenefitNotFound", err)
	}
}

func TestClaimNoMezclaLasCampañas(t *testing.T) {
	repo, _ := newRepo(t)
	a, b := newCampaign(t, repo), newCampaign(t, repo)
	addCodes(t, repo, a.ID, "CAMPA-0001")
	addCodes(t, repo, b.ID, "CAMPB-0001")
	user := uuid.New()

	codeA, errA := repo.Claim(context.Background(), a.ID, user, fixedNow)
	codeB, errB := repo.Claim(context.Background(), b.ID, user, fixedNow)

	if errA != nil || errB != nil {
		t.Fatalf("un usuario puede tener un código por campaña: %v, %v", errA, errB)
	}
	if codeA != "enc:CAMPA-0001" || codeB != "enc:CAMPB-0001" {
		t.Errorf("códigos = %q y %q; cada campaña entrega los suyos", codeA, codeB)
	}
}

// Dos usuarios nunca reciben el mismo código, ni con pedidos concurrentes: más
// pedidores que códigos, todos a la vez.
func TestClaimConcurrenteNuncaEntregaElMismoCodigoADosPersonas(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	const codigos, pedidores = 20, 60
	addCodes(t, repo, campaign.ID, numberedCodes("CONC", codigos)...)

	type resultado struct {
		code string
		err  error
	}
	resultados := make([]resultado, pedidores)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < pedidores; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			code, err := repo.Claim(context.Background(), campaign.ID, uuid.New(), fixedNow)
			resultados[i] = resultado{code, err}
		}()
	}
	close(start)
	wg.Wait()

	entregados := map[string]int{}
	agotados := 0
	for _, r := range resultados {
		switch {
		case r.err == nil:
			entregados[r.code]++
		case errors.Is(r.err, domain.ErrNoCodesLeft):
			agotados++
		default:
			t.Fatalf("error inesperado: %v", r.err)
		}
	}
	for code, veces := range entregados {
		if veces != 1 {
			t.Errorf("el código %q se entregó %d veces", code, veces)
		}
	}
	if len(entregados) != codigos || agotados != pedidores-codigos {
		t.Errorf("entregados = %d y agotados = %d; esperaba %d y %d", len(entregados), agotados, codigos, pedidores-codigos)
	}
	if stats := statsOf(t, repo, campaign.ID); stats.Assigned != codigos || stats.Available != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

// Dos pedidos simultáneos de la misma persona devuelven el mismo código y consumen uno.
func TestClaimConcurrenteDeLaMismaPersonaDevuelveElMismoCodigo(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, numberedCodes("MISMO", 10)...)
	user := uuid.New()
	const pedidos = 25

	codes := make([]string, pedidos)
	errs := make([]error, pedidos)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < pedidos; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i], errs[i] = repo.Claim(context.Background(), campaign.ID, user, fixedNow)
		}()
	}
	close(start)
	wg.Wait()

	for i := range codes {
		if errs[i] != nil {
			t.Fatalf("pedido %d: %v", i, errs[i])
		}
		if codes[i] != codes[0] {
			t.Fatalf("el pedido %d recibió %q y el 0 recibió %q", i, codes[i], codes[0])
		}
	}
	if stats := statsOf(t, repo, campaign.ID); stats.Assigned != 1 || stats.Available != 9 {
		t.Errorf("stats = %+v; pedidos simultáneos de una persona consumen un solo código", stats)
	}
}

// --- ListLive --------------------------------------------------------------

func idsOf(benefits []domain.Benefit) []uuid.UUID {
	ids := make([]uuid.UUID, len(benefits))
	for i, b := range benefits {
		ids[i] = b.ID
	}
	return ids
}

func TestListLiveMuestraSoloLasCampañasVigentesConCodigos(t *testing.T) {
	repo, _ := newRepo(t)
	vigente := newCampaign(t, repo)
	addCodes(t, repo, vigente.ID, "LIST-0001")

	inactiva := newCampaign(t, repo, func(c *domain.Campaign) { c.Active = false })
	addCodes(t, repo, inactiva.ID, "INAC-0001")
	vencida := newCampaign(t, repo, func(c *domain.Campaign) {
		c.StartsAt, c.EndsAt = fixedNow.Add(-48*time.Hour), fixedNow.Add(-24*time.Hour)
	})
	addCodes(t, repo, vencida.ID, "VENC-0001")
	futura := newCampaign(t, repo, func(c *domain.Campaign) {
		c.StartsAt, c.EndsAt = fixedNow.Add(24*time.Hour), fixedNow.Add(48*time.Hour)
	})
	addCodes(t, repo, futura.ID, "FUTU-0001")
	sinCodigos := newCampaign(t, repo)

	benefits, err := repo.ListLive(context.Background(), uuid.New(), fixedNow)
	if err != nil {
		t.Fatalf("ListLive: %v", err)
	}

	if ids := idsOf(benefits); len(ids) != 1 || ids[0] != vigente.ID {
		t.Fatalf("ids = %v; esperaba solo la campaña vigente %v (no la inactiva, la vencida, la futura ni la sin códigos %v)", ids, vigente.ID, sinCodigos.ID)
	}
	got := benefits[0]
	if got.PartnerName != vigente.PartnerName || got.Title != vigente.Title || got.Terms != vigente.Terms ||
		got.OpenURL != vigente.OpenURL || !got.EndsAt.Equal(vigente.EndsAt) || got.CodeEnc != "" {
		t.Errorf("campos incorrectos o con código antes de pedirlo: %+v", got)
	}
}

func TestListLiveDevuelveElCodigoSoloAlQueLoPidio(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, "PROP-0001", "PROP-0002")
	conCodigo, sinCodigo := uuid.New(), uuid.New()
	asignado, err := repo.Claim(context.Background(), campaign.ID, conCodigo, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	mio, err := repo.ListLive(context.Background(), conCodigo, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	ajeno, err := repo.ListLive(context.Background(), sinCodigo, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	if len(mio) != 1 || mio[0].CodeEnc != asignado {
		t.Errorf("el dueño debía ver su código %q: %+v", asignado, mio)
	}
	if len(ajeno) != 1 || ajeno[0].CodeEnc != "" {
		t.Errorf("otro usuario no debe ver el código de nadie: %+v", ajeno)
	}
}

func TestListLiveConElPoolAgotadoSoloLaVeQuienYaTieneCodigo(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, "AGOT-0001")
	titular := uuid.New()
	if _, err := repo.Claim(context.Background(), campaign.ID, titular, fixedNow); err != nil {
		t.Fatal(err)
	}

	paraElTitular, err := repo.ListLive(context.Background(), titular, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	paraOtro, err := repo.ListLive(context.Background(), uuid.New(), fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	if len(paraElTitular) != 1 {
		t.Errorf("el que ya tiene código sigue viendo la campaña agotada: %v", idsOf(paraElTitular))
	}
	if len(paraOtro) != 0 {
		t.Errorf("sin códigos disponibles y sin código propio no se muestra: %v", idsOf(paraOtro))
	}
}

func TestListLiveOrdenaPorEndsAt(t *testing.T) {
	repo, _ := newRepo(t)
	tarde := newCampaign(t, repo, func(c *domain.Campaign) { c.EndsAt = fixedNow.Add(72 * time.Hour) })
	pronto := newCampaign(t, repo, func(c *domain.Campaign) { c.EndsAt = fixedNow.Add(2 * time.Hour) })
	medio := newCampaign(t, repo, func(c *domain.Campaign) { c.EndsAt = fixedNow.Add(24 * time.Hour) })
	for _, c := range []domain.Campaign{tarde, pronto, medio} {
		addCodes(t, repo, c.ID, "ORD-"+uuid.NewString()[:6])
	}

	benefits, err := repo.ListLive(context.Background(), uuid.New(), fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	want := []uuid.UUID{pronto.ID, medio.ID, tarde.ID}
	got := idsOf(benefits)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("orden = %v, esperaba %v (por ends_at ascendente)", got, want)
	}
}

func TestListLiveSinCampañasDevuelveUnaListaVacia(t *testing.T) {
	repo, _ := newRepo(t)

	benefits, err := repo.ListLive(context.Background(), uuid.New(), fixedNow)

	if err != nil || benefits == nil || len(benefits) != 0 {
		t.Fatalf("ListLive = %v, %v; esperaba una lista vacía no nula", benefits, err)
	}
}

// --- Banner ----------------------------------------------------------------

func TestBannerDevuelveLaImagenDeUnaCampañaVigente(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	esperado := fakeBanner(campaign.ID.String())

	meta, err := repo.BannerMeta(context.Background(), campaign.ID, fixedNow)
	if err != nil {
		t.Fatalf("BannerMeta: %v", err)
	}
	full, err := repo.BannerData(context.Background(), campaign.ID, fixedNow)
	if err != nil {
		t.Fatalf("BannerData: %v", err)
	}

	if meta.ContentType != esperado.ContentType || meta.ETag != esperado.ETag || len(meta.Data) != 0 {
		t.Errorf("meta = %+v; esperaba tipo y ETag sin bytes", meta)
	}
	if full.ContentType != esperado.ContentType || full.ETag != esperado.ETag || string(full.Data) != string(esperado.Data) {
		t.Errorf("banner = %+v, esperaba %+v", full, esperado)
	}
}

func TestBannerDeUnaCampañaNoVigenteODesconocidaEsBenefitNotFound(t *testing.T) {
	repo, _ := newRepo(t)
	inactiva := newCampaign(t, repo, func(c *domain.Campaign) { c.Active = false })
	vencida := newCampaign(t, repo, func(c *domain.Campaign) {
		c.StartsAt, c.EndsAt = fixedNow.Add(-2*time.Hour), fixedNow.Add(-time.Hour)
	})

	for nombre, id := range map[string]uuid.UUID{"inactiva": inactiva.ID, "vencida": vencida.ID, "inexistente": uuid.New()} {
		t.Run(nombre, func(t *testing.T) {
			if _, err := repo.BannerMeta(context.Background(), id, fixedNow); !errors.Is(err, domain.ErrBenefitNotFound) {
				t.Errorf("BannerMeta: error = %v, esperaba ErrBenefitNotFound", err)
			}
			if _, err := repo.BannerData(context.Background(), id, fixedNow); !errors.Is(err, domain.ErrBenefitNotFound) {
				t.Errorf("BannerData: error = %v, esperaba ErrBenefitNotFound", err)
			}
		})
	}
}

// --- Baja de cuenta --------------------------------------------------------

func TestBurnByUserQuemaLosCodigosDelUsuarioYNoLosDevuelveAlPool(t *testing.T) {
	repo, db := newRepo(t)
	campaign := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, numberedCodes("BURN", 4)...)
	baja, otro := uuid.New(), uuid.New()
	if _, err := repo.Claim(context.Background(), campaign.ID, baja, fixedNow); err != nil {
		t.Fatal(err)
	}
	codigoDelOtro, err := repo.Claim(context.Background(), campaign.ID, otro, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	burned, err := repo.BurnByUser(context.Background(), baja)

	if err != nil || burned != 1 {
		t.Fatalf("BurnByUser = %d, %v; esperaba 1", burned, err)
	}
	stats := statsOf(t, repo, campaign.ID)
	if stats.Burned != 1 || stats.Assigned != 1 || stats.Available != 2 {
		t.Errorf("stats = %+v; el código quemado no vuelve al pool y el del otro sigue asignado", stats)
	}
	var conUsuario int64
	if err := db.Raw(`SELECT count(*) FROM codes WHERE status = 'BURNED' AND assigned_to IS NOT NULL`).Scan(&conUsuario).Error; err != nil || conUsuario != 0 {
		t.Errorf("un código quemado no debe conservar assigned_to (%d, %v)", conUsuario, err)
	}
	if again, err := repo.Claim(context.Background(), campaign.ID, otro, fixedNow); err != nil || again != codigoDelOtro {
		t.Errorf("el otro usuario conserva su código: %q, %v", again, err)
	}
}

func TestBurnByUserEsIdempotenteYNoTocaAOtros(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, numberedCodes("IDEM", 3)...)
	baja, otro := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{baja, otro} {
		if _, err := repo.Claim(context.Background(), campaign.ID, u, fixedNow); err != nil {
			t.Fatal(err)
		}
	}

	primero, err1 := repo.BurnByUser(context.Background(), baja)
	segundo, err2 := repo.BurnByUser(context.Background(), baja)
	sinCodigos, err3 := repo.BurnByUser(context.Background(), uuid.New())

	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatalf("errores inesperados: %v, %v, %v", err1, err2, err3)
	}
	if primero != 1 || segundo != 0 || sinCodigos != 0 {
		t.Errorf("quemados = %d, %d, %d; esperaba 1, 0, 0", primero, segundo, sinCodigos)
	}
	if stats := statsOf(t, repo, campaign.ID); stats.Burned != 1 || stats.Assigned != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestBurnByUserQuemaLosCodigosDeTodasLasCampañas(t *testing.T) {
	repo, _ := newRepo(t)
	a, b := newCampaign(t, repo), newCampaign(t, repo)
	addCodes(t, repo, a.ID, "TODAA-0001")
	addCodes(t, repo, b.ID, "TODAB-0001")
	user := uuid.New()
	for _, c := range []domain.Campaign{a, b} {
		if _, err := repo.Claim(context.Background(), c.ID, user, fixedNow); err != nil {
			t.Fatal(err)
		}
	}

	burned, err := repo.BurnByUser(context.Background(), user)

	if err != nil || burned != 2 {
		t.Fatalf("BurnByUser = %d, %v; esperaba 2", burned, err)
	}
}

// --- Administración --------------------------------------------------------

func TestCreateYListCampaignsConEstadisticas(t *testing.T) {
	repo, _ := newRepo(t)
	vieja := newCampaign(t, repo, func(c *domain.Campaign) { c.CreatedAt = fixedNow.Add(-time.Hour) })
	nueva := newCampaign(t, repo)
	addCodes(t, repo, nueva.ID, numberedCodes("LST", 5)...)
	user := uuid.New()
	if _, err := repo.Claim(context.Background(), nueva.ID, user, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Claim(context.Background(), nueva.ID, uuid.New(), fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.BurnByUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkRedeemed(context.Background(), nueva.ID, []string{fakeHMAC("LST-0001"), fakeHMAC("LST-0002"), fakeHMAC("LST-0003")}, fixedNow); err != nil {
		t.Fatal(err)
	}

	summaries, err := repo.ListCampaigns(context.Background())
	if err != nil {
		t.Fatalf("ListCampaigns: %v", err)
	}

	if len(summaries) != 2 || summaries[0].ID != nueva.ID || summaries[1].ID != vieja.ID {
		t.Fatalf("orden = %v; esperaba la más nueva primero", summaryIDs(summaries))
	}
	got := summaries[0]
	if got.Stats != (domain.CampaignStats{Available: 3, Assigned: 1, Burned: 1, Redeemed: 3}) {
		t.Errorf("stats = %+v", got.Stats)
	}
	if got.BannerETag != fakeBanner(nueva.ID.String()).ETag || got.PartnerName != nueva.PartnerName || !got.Active {
		t.Errorf("campos incorrectos: %+v", got)
	}
	if summaries[1].Stats != (domain.CampaignStats{}) {
		t.Errorf("una campaña sin códigos tiene contadores en cero: %+v", summaries[1].Stats)
	}
}

func summaryIDs(summaries []domain.CampaignSummary) []uuid.UUID {
	ids := make([]uuid.UUID, len(summaries))
	for i, s := range summaries {
		ids[i] = s.ID
	}
	return ids
}

func TestUpdateCampaignAplicaLaMutacionBajoLock(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	later := fixedNow.Add(time.Hour)

	err := repo.UpdateCampaign(context.Background(), campaign.ID, later, func(c domain.Campaign) (domain.Campaign, error) {
		if c.Title != campaign.Title {
			t.Errorf("la mutación debía recibir la campaña vigente: %+v", c)
		}
		c.Title = "Nuevo título"
		c.Active = false
		return c, nil
	})
	if err != nil {
		t.Fatalf("UpdateCampaign: %v", err)
	}

	summaries, err := repo.ListCampaigns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := summaries[0]
	if got.Title != "Nuevo título" || got.Active || !got.UpdatedAt.Equal(later) || !got.CreatedAt.Equal(fixedNow) {
		t.Errorf("campaña = %+v; esperaba título y estado nuevos, updated_at movido y created_at intacto", got)
	}
}

func TestUpdateCampaignSiLaMutacionFallaNoCambiaNadaYDevuelveElError(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)

	err := repo.UpdateCampaign(context.Background(), campaign.ID, fixedNow.Add(time.Hour), func(c domain.Campaign) (domain.Campaign, error) {
		c.Title = "no debe guardarse"
		return c, domain.ErrInvalidCampaign
	})

	if !errors.Is(err, domain.ErrInvalidCampaign) {
		t.Fatalf("error = %v, esperaba ErrInvalidCampaign", err)
	}
	summaries, _ := repo.ListCampaigns(context.Background())
	if summaries[0].Title != campaign.Title {
		t.Errorf("título = %q; una mutación rechazada no debe guardarse", summaries[0].Title)
	}
}

func TestUpdateCampaignInexistente(t *testing.T) {
	repo, _ := newRepo(t)

	err := repo.UpdateCampaign(context.Background(), uuid.New(), fixedNow, func(c domain.Campaign) (domain.Campaign, error) { return c, nil })

	if !errors.Is(err, domain.ErrCampaignNotFound) {
		t.Fatalf("error = %v, esperaba ErrCampaignNotFound", err)
	}
}

func TestUpdateCampaignConcurrenteSerializaLosCambios(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, "SERIAL-1")
	const parches = 10

	var wg sync.WaitGroup
	for i := 0; i < parches; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := repo.UpdateCampaign(context.Background(), campaign.ID, fixedNow, func(c domain.Campaign) (domain.Campaign, error) {
				c.Description += "x"
				return c, nil
			})
			if err != nil {
				t.Errorf("UpdateCampaign: %v", err)
			}
		}()
	}
	wg.Wait()

	summaries, _ := repo.ListCampaigns(context.Background())
	if got, want := len(summaries[0].Description), len(campaign.Description)+parches; got != want {
		t.Errorf("largo de la descripción = %d, esperaba %d: se perdió una actualización", got, want)
	}
}

func TestReplaceBanner(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	nuevo := fakeBanner("otra-imagen")
	nuevo.ContentType = "image/jpeg"

	if err := repo.ReplaceBanner(context.Background(), campaign.ID, nuevo, fixedNow.Add(time.Hour)); err != nil {
		t.Fatalf("ReplaceBanner: %v", err)
	}

	got, err := repo.BannerData(context.Background(), campaign.ID, fixedNow)
	if err != nil || got.ETag != nuevo.ETag || got.ContentType != "image/jpeg" || string(got.Data) != string(nuevo.Data) {
		t.Errorf("banner = %+v, %v; esperaba el nuevo", got, err)
	}
	if err := repo.ReplaceBanner(context.Background(), uuid.New(), nuevo, fixedNow); !errors.Is(err, domain.ErrCampaignNotFound) {
		t.Errorf("campaña inexistente: error = %v, esperaba ErrCampaignNotFound", err)
	}
}

func TestInsertCodesIgnoraLosRepetidosYContabilizaLosInsertados(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, "REP-0001", "REP-0002")

	lote := []domain.NewCode{
		{Enc: "enc:REP-0002", HMAC: fakeHMAC("REP-0002")},
		{Enc: "enc:REP-0003", HMAC: fakeHMAC("REP-0003")},
		{Enc: "enc:REP-0004", HMAC: fakeHMAC("REP-0004")},
	}
	inserted, err := repo.InsertCodes(context.Background(), campaign.ID, lote)

	if err != nil || inserted != 2 {
		t.Fatalf("InsertCodes = %d, %v; esperaba 2 (el REP-0002 ya existía)", inserted, err)
	}
	if stats := statsOf(t, repo, campaign.ID); stats.Available != 4 {
		t.Errorf("stats = %+v, esperaba 4 disponibles", stats)
	}
}

func TestInsertCodesEnVariosLotes(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	total := batchSize*2 + 37

	inserted, err := repo.InsertCodes(context.Background(), campaign.ID, newBatch(total))

	if err != nil || inserted != total {
		t.Fatalf("InsertCodes = %d, %v; esperaba %d", inserted, err, total)
	}
	if stats := statsOf(t, repo, campaign.ID); stats.Available != int64(total) {
		t.Errorf("stats = %+v", stats)
	}
}

func newBatch(n int) []domain.NewCode {
	batch := make([]domain.NewCode, n)
	for i := range batch {
		code := fmt.Sprintf("LOTE-%06d", i)
		batch[i] = domain.NewCode{Enc: "enc:" + code, HMAC: fakeHMAC(code)}
	}
	return batch
}

func TestInsertCodesEnUnaCampañaInexistenteNoInsertaNada(t *testing.T) {
	repo, db := newRepo(t)

	inserted, err := repo.InsertCodes(context.Background(), uuid.New(), newBatch(3))

	if !errors.Is(err, domain.ErrCampaignNotFound) || inserted != 0 {
		t.Fatalf("InsertCodes = %d, %v; esperaba 0 y ErrCampaignNotFound", inserted, err)
	}
	var filas int64
	_ = db.Raw(`SELECT count(*) FROM codes`).Scan(&filas).Error
	if filas != 0 {
		t.Errorf("no debía quedar ninguna fila, hay %d", filas)
	}
}

func TestMarkRedeemed(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	other := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, "CANJ-0001", "CANJ-0002", "CANJ-0003")
	addCodes(t, repo, other.ID, "CANJ-0001")
	primero := fixedNow
	despues := fixedNow.Add(48 * time.Hour)

	matched, err := repo.MarkRedeemed(context.Background(), campaign.ID,
		[]string{fakeHMAC("CANJ-0001"), fakeHMAC("CANJ-9999")}, primero)
	if err != nil || matched != 1 {
		t.Fatalf("MarkRedeemed = %d, %v; esperaba 1 (el otro no existe)", matched, err)
	}

	// Reportar de nuevo el mismo canje no lo mueve de fecha, pero sigue emparejando.
	matched, err = repo.MarkRedeemed(context.Background(), campaign.ID, []string{fakeHMAC("CANJ-0001"), fakeHMAC("CANJ-0002")}, despues)
	if err != nil || matched != 2 {
		t.Fatalf("segundo reporte = %d, %v; esperaba 2", matched, err)
	}

	if stats := statsOf(t, repo, campaign.ID); stats.Redeemed != 2 {
		t.Errorf("stats = %+v; esperaba 2 canjeados", stats)
	}
	if stats := statsOf(t, repo, other.ID); stats.Redeemed != 0 {
		t.Errorf("el mismo código de otra campaña no se canjea: %+v", stats)
	}
	var fechas []time.Time
	if err := repo.db.Raw(`SELECT redeemed_at FROM codes WHERE code_hmac = ? AND campaign_id = ?`, fakeHMAC("CANJ-0001"), campaign.ID).Scan(&fechas).Error; err != nil {
		t.Fatal(err)
	}
	if len(fechas) != 1 || !fechas[0].Equal(primero) {
		t.Errorf("redeemed_at = %v; debe conservar la primera fecha %v", fechas, primero)
	}
}

func TestMarkRedeemedMarcaTambienLosCodigosQuemados(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	addCodes(t, repo, campaign.ID, "QUEM-0001")
	user := uuid.New()
	if _, err := repo.Claim(context.Background(), campaign.ID, user, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.BurnByUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}

	matched, err := repo.MarkRedeemed(context.Background(), campaign.ID, []string{fakeHMAC("QUEM-0001")}, fixedNow)

	if err != nil || matched != 1 {
		t.Fatalf("MarkRedeemed = %d, %v; el usuario pudo canjear antes de la baja", matched, err)
	}
	if stats := statsOf(t, repo, campaign.ID); stats.Burned != 1 || stats.Redeemed != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestMarkRedeemedEnVariosLotes(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)
	total := batchSize + 50
	batch := newBatch(total)
	if _, err := repo.InsertCodes(context.Background(), campaign.ID, batch); err != nil {
		t.Fatal(err)
	}
	hmacs := make([]string, len(batch))
	for i, c := range batch {
		hmacs[i] = c.HMAC
	}

	matched, err := repo.MarkRedeemed(context.Background(), campaign.ID, hmacs, fixedNow)

	if err != nil || matched != total {
		t.Fatalf("MarkRedeemed = %d, %v; esperaba %d", matched, err, total)
	}
}

func TestMarkRedeemedSinCodigosNoHaceNada(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)

	matched, err := repo.MarkRedeemed(context.Background(), campaign.ID, nil, fixedNow)

	if err != nil || matched != 0 {
		t.Fatalf("MarkRedeemed = %d, %v; esperaba 0", matched, err)
	}
}

func TestMarkRedeemedEnUnaCampañaInexistente(t *testing.T) {
	repo, _ := newRepo(t)

	if _, err := repo.MarkRedeemed(context.Background(), uuid.New(), []string{fakeHMAC("X")}, fixedNow); !errors.Is(err, domain.ErrCampaignNotFound) {
		t.Fatalf("error = %v, esperaba ErrCampaignNotFound", err)
	}
}

func TestStatsDeUnaCampañaInexistente(t *testing.T) {
	repo, _ := newRepo(t)

	if _, err := repo.Stats(context.Background(), uuid.New()); !errors.Is(err, domain.ErrCampaignNotFound) {
		t.Fatalf("error = %v, esperaba ErrCampaignNotFound", err)
	}
}

func TestStatsDeUnaCampañaSinCodigosEsCero(t *testing.T) {
	repo, _ := newRepo(t)
	campaign := newCampaign(t, repo)

	if stats := statsOf(t, repo, campaign.ID); stats != (domain.CampaignStats{}) {
		t.Errorf("stats = %+v, esperaba todo en cero", stats)
	}
}

// --- Disponibilidad --------------------------------------------------------

func TestUnPlazoVencidoSeReportaComoIndisponibilidad(t *testing.T) {
	repo, _ := newRepo(t, WithQueryTimeout(time.Nanosecond), WithAdminTimeout(time.Nanosecond))
	id := uuid.New()
	ctx := context.Background()

	llamadas := map[string]func() error{
		"Ping":           func() error { return repo.Ping(ctx) },
		"ListLive":       func() error { _, err := repo.ListLive(ctx, id, fixedNow); return err },
		"BannerMeta":     func() error { _, err := repo.BannerMeta(ctx, id, fixedNow); return err },
		"BannerData":     func() error { _, err := repo.BannerData(ctx, id, fixedNow); return err },
		"Claim":          func() error { _, err := repo.Claim(ctx, id, id, fixedNow); return err },
		"BurnByUser":     func() error { _, err := repo.BurnByUser(ctx, id); return err },
		"CreateCampaign": func() error { return repo.CreateCampaign(ctx, domain.Campaign{ID: id}, fakeBanner("x")) },
		"ListCampaigns":  func() error { _, err := repo.ListCampaigns(ctx); return err },
		"UpdateCampaign": func() error {
			return repo.UpdateCampaign(ctx, id, fixedNow, func(c domain.Campaign) (domain.Campaign, error) { return c, nil })
		},
		"ReplaceBanner": func() error { return repo.ReplaceBanner(ctx, id, fakeBanner("x"), fixedNow) },
		"InsertCodes":   func() error { _, err := repo.InsertCodes(ctx, id, newBatch(1)); return err },
		"MarkRedeemed":  func() error { _, err := repo.MarkRedeemed(ctx, id, []string{"x"}, fixedNow); return err },
		"Stats":         func() error { _, err := repo.Stats(ctx, id); return err },
	}
	nombres := make([]string, 0, len(llamadas))
	for nombre := range llamadas {
		nombres = append(nombres, nombre)
	}
	sort.Strings(nombres)

	for _, nombre := range nombres {
		t.Run(nombre, func(t *testing.T) {
			if err := llamadas[nombre](); !errors.Is(err, domain.ErrRepositoryUnavailable) {
				t.Fatalf("error = %v, esperaba ErrRepositoryUnavailable", err)
			}
		})
	}
}

func TestPingConUnaBaseVivaResponde(t *testing.T) {
	repo, _ := newRepo(t)

	if err := repo.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestOpcionesDeTimeout(t *testing.T) {
	repo, _ := newRepo(t, WithQueryTimeout(3*time.Second), WithAdminTimeout(4*time.Minute))

	if repo.queryTimeout != 3*time.Second || repo.adminTimeout != 4*time.Minute {
		t.Errorf("timeouts = %v y %v", repo.queryTimeout, repo.adminTimeout)
	}
	predeterminado := NewRepository(repo.db)
	if predeterminado.queryTimeout != defaultQueryTimeout || predeterminado.adminTimeout != defaultAdminTimeout {
		t.Errorf("los valores por defecto no se aplicaron: %v y %v", predeterminado.queryTimeout, predeterminado.adminTimeout)
	}
}

func TestConnectConUnaBaseInalcanzableFalla(t *testing.T) {
	if _, err := Connect("postgres://nadie:nada@127.0.0.1:1/nada?sslmode=disable&connect_timeout=1"); err == nil {
		t.Fatal("esperaba un error al conectar a una base inexistente")
	}
}

func TestConnectConUnaCadenaIlegibleFalla(t *testing.T) {
	if _, err := Connect("postgres://\x7f"); err == nil {
		t.Fatal("esperaba un error con una cadena de conexión ilegible")
	}
}

// El logger por defecto de GORM imprime las consultas lentas con sus parámetros: una carga
// de códigos volcaría code_enc al log. Connect lo apaga.
func TestConnectNoDejaQueGormLogueeElSQLConSusParametros(t *testing.T) {
	_, db := newRepo(t)

	if db.Logger != gormlogger.Discard {
		t.Fatalf("el logger de GORM es %T; debe descartar todo para no filtrar códigos cifrados", db.Logger)
	}
}
