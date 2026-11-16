package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validCampaign() Campaign {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	return Campaign{
		ID:          uuid.New(),
		PartnerName: "Petrolera Sur",
		Title:       "10 % de descuento",
		Description: "Descuento en combustible",
		Terms:       "Un código por persona",
		OpenURL:     "https://partner.example.com/canje",
		StartsAt:    start,
		EndsAt:      start.Add(30 * 24 * time.Hour),
		Active:      true,
	}
}

func TestCampaignValidateAceptaUnaCampañaValida(t *testing.T) {
	if err := validCampaign().Validate(); err != nil {
		t.Fatalf("error inesperado: %v", err)
	}
}

func TestCampaignValidateRechazaLosCamposInvalidos(t *testing.T) {
	casos := map[string]struct {
		mutar func(*Campaign)
		campo string
	}{
		"partner vacío":        {func(c *Campaign) { c.PartnerName = "  " }, "partner_name"},
		"partner largo":        {func(c *Campaign) { c.PartnerName = strings.Repeat("a", 81) }, "partner_name"},
		"título vacío":         {func(c *Campaign) { c.Title = "" }, "title"},
		"título largo":         {func(c *Campaign) { c.Title = strings.Repeat("a", 81) }, "title"},
		"descripción vacía":    {func(c *Campaign) { c.Description = "" }, "description"},
		"descripción larga":    {func(c *Campaign) { c.Description = strings.Repeat("a", 501) }, "description"},
		"condiciones vacías":   {func(c *Campaign) { c.Terms = "" }, "terms"},
		"condiciones largas":   {func(c *Campaign) { c.Terms = strings.Repeat("a", 2001) }, "terms"},
		"UTF-8 inválido":       {func(c *Campaign) { c.Title = "mal\xff" }, "title"},
		"sin starts_at":        {func(c *Campaign) { c.StartsAt = time.Time{} }, "starts_at"},
		"sin ends_at":          {func(c *Campaign) { c.EndsAt = time.Time{} }, "ends_at"},
		"ends_at == starts_at": {func(c *Campaign) { c.EndsAt = c.StartsAt }, "ends_at"},
		"ends_at < starts_at":  {func(c *Campaign) { c.EndsAt = c.StartsAt.Add(-time.Hour) }, "ends_at"},
		"URL vacía":            {func(c *Campaign) { c.OpenURL = "" }, "open_url"},
		"URL http":             {func(c *Campaign) { c.OpenURL = "http://partner.example.com" }, "open_url"},
	}

	for nombre, caso := range casos {
		t.Run(nombre, func(t *testing.T) {
			campaign := validCampaign()
			caso.mutar(&campaign)

			err := campaign.Validate()

			if !errors.Is(err, ErrInvalidCampaign) {
				t.Fatalf("error = %v, esperaba que envolviera ErrInvalidCampaign", err)
			}
			if !strings.Contains(err.Error(), caso.campo) {
				t.Errorf("error = %q, esperaba que nombrara el campo %s", err, caso.campo)
			}
		})
	}
}

func TestCampaignValidateContandoCaracteresYNoBytes(t *testing.T) {
	campaign := validCampaign()
	// 80 caracteres de 2 bytes cada uno: cabe en varchar(80) aunque pese 160 bytes.
	campaign.Title = strings.Repeat("ñ", 80)

	if err := campaign.Validate(); err != nil {
		t.Fatalf("80 caracteres deben caber: %v", err)
	}
}

func TestValidateOpenURL(t *testing.T) {
	validas := []string{
		"https://partner.example.com",
		"https://partner.example.com/app?campania=vv",
		"https://play.google.com/store/apps/details?id=com.partner",
		"https://partner.example.com:8443/x",
	}
	for _, raw := range validas {
		if err := ValidateOpenURL(raw); err != nil {
			t.Errorf("%q debía ser válida: %v", raw, err)
		}
	}

	// url.Parse normaliza el esquema a minúsculas, así que HTTPS:// es https.
	if err := ValidateOpenURL("HTTPS://partner.example.com/x"); err != nil {
		t.Errorf("el esquema se compara sin distinguir mayúsculas: %v", err)
	}

	invalidas := map[string]string{
		"http":             "http://partner.example.com",
		"intent":           "intent://scan/#Intent;scheme=zxing;end",
		"javascript":       "javascript:alert(1)",
		"sin esquema":      "partner.example.com/x",
		"sin host":         "https:///solo-ruta",
		"con credenciales": "https://usuario:clave@partner.example.com",
		"con usuario":      "https://partner.example.com@evil.example.org",
		"ilegible":         "https://%zz",
		"con espacio":      "https://partner example.com",
		"demasiado larga":  "https://partner.example.com/" + strings.Repeat("a", 500),
		"vacía":            "",
	}
	for nombre, raw := range invalidas {
		if err := ValidateOpenURL(raw); !errors.Is(err, ErrInvalidCampaign) {
			t.Errorf("%s (%q): error = %v, esperaba ErrInvalidCampaign", nombre, raw, err)
		}
	}
}

func TestIsLive(t *testing.T) {
	campaign := validCampaign()
	dentro := campaign.StartsAt.Add(time.Hour)

	casos := map[string]struct {
		activa  bool
		ahora   time.Time
		vigente bool
	}{
		"activa y dentro del rango":     {true, dentro, true},
		"inactiva":                      {false, dentro, false},
		"antes de empezar":              {true, campaign.StartsAt.Add(-time.Second), false},
		"justo al empezar":              {true, campaign.StartsAt, true},
		"justo al terminar":             {true, campaign.EndsAt, false},
		"un instante antes de terminar": {true, campaign.EndsAt.Add(-time.Nanosecond), true},
		"después de terminar":           {true, campaign.EndsAt.Add(time.Hour), false},
	}
	for nombre, caso := range casos {
		t.Run(nombre, func(t *testing.T) {
			campaign.Active = caso.activa
			if got := campaign.IsLive(caso.ahora); got != caso.vigente {
				t.Errorf("IsLive = %v, esperaba %v", got, caso.vigente)
			}
		})
	}
}

func TestValidCode(t *testing.T) {
	validos := []string{"ABCD", "abcd", "AB-12", "0123", strings.Repeat("a", 64), "PRUEBA-0001", "a-b-c-d"}
	for _, code := range validos {
		if !ValidCode(code) {
			t.Errorf("%q debía ser válido", code)
		}
	}

	invalidos := map[string]string{
		"corto":            "abc",
		"vacío":            "",
		"largo":            strings.Repeat("a", 65),
		"con espacio":      "AB CD",
		"con guion bajo":   "AB_CD",
		"con punto":        "AB.CD",
		"con coma":         "AB,CD",
		"con ñ":            "AÑOS1",
		"con salto":        "AB\nCD",
		"con comilla":      "AB'CD",
		"con punto y coma": "AB;CD",
	}
	for nombre, code := range invalidos {
		if ValidCode(code) {
			t.Errorf("%s (%q) no debía ser válido", nombre, code)
		}
	}
}

func TestCampaignPatch(t *testing.T) {
	base := validCampaign()
	nuevoTitulo, nuevaURL := "Otro título", "https://otro.example.com"
	activa := false
	fin := base.EndsAt.Add(24 * time.Hour)

	patch := CampaignPatch{Title: &nuevoTitulo, OpenURL: &nuevaURL, Active: &activa, EndsAt: &fin}
	got := patch.Apply(base)

	if got.Title != nuevoTitulo || got.OpenURL != nuevaURL || got.Active || !got.EndsAt.Equal(fin) {
		t.Errorf("el parche no se aplicó bien: %+v", got)
	}
	if got.PartnerName != base.PartnerName || got.Description != base.Description || got.Terms != base.Terms || !got.StartsAt.Equal(base.StartsAt) {
		t.Errorf("el parche tocó campos que no traía: %+v", got)
	}

	todos := CampaignPatch{Description: &nuevoTitulo, Terms: &nuevoTitulo, StartsAt: &fin}
	got = todos.Apply(base)
	if got.Description != nuevoTitulo || got.Terms != nuevoTitulo || !got.StartsAt.Equal(fin) {
		t.Errorf("descripción, condiciones y starts_at no se aplicaron: %+v", got)
	}
}

func TestCampaignPatchIsEmpty(t *testing.T) {
	if !(CampaignPatch{}).IsEmpty() {
		t.Error("un parche sin campos está vacío")
	}
	valor := "x"
	activa := true
	fecha := time.Now()
	for nombre, patch := range map[string]CampaignPatch{
		"title":       {Title: &valor},
		"description": {Description: &valor},
		"terms":       {Terms: &valor},
		"open_url":    {OpenURL: &valor},
		"starts_at":   {StartsAt: &fecha},
		"ends_at":     {EndsAt: &fecha},
		"active":      {Active: &activa},
	} {
		if patch.IsEmpty() {
			t.Errorf("un parche con %s no está vacío", nombre)
		}
	}
}
