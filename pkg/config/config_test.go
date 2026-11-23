package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// key32 devuelve una clave válida (32 bytes en base64) cuyo contenido depende de seed.
func key32(seed byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// validEnv es el entorno mínimo válido.
func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":           "postgres://benefits:benefits@localhost:5432/benefits?sslmode=disable",
		"RABBITMQ_URL":           "amqp://guest:guest@localhost:5672/",
		"BENEFITS_CODE_AES_KEY":  key32(1),
		"BENEFITS_CODE_HMAC_KEY": key32(100),
	}
}

func loadFrom(env map[string]string) (*Config, error) {
	return LoadFrom(func(name string) string { return env[name] })
}

func TestLoadAplicaLosValoresPorDefectoDelContrato(t *testing.T) {
	cfg, err := loadFrom(validEnv())
	if err != nil {
		t.Fatalf("error inesperado: %v", err)
	}

	if cfg.Port != "8080" || cfg.AdminPort != "8082" {
		t.Errorf("puertos = %s y %s, esperaba 8080 y 8082", cfg.Port, cfg.AdminPort)
	}
	if cfg.UserDeletedQueue != "benefits.user-deleted.q" {
		t.Errorf("cola = %q, esperaba benefits.user-deleted.q", cfg.UserDeletedQueue)
	}
	if cfg.BannerMaxBytes != 307200 {
		t.Errorf("BANNER_MAX_BYTES = %d, esperaba 307200", cfg.BannerMaxBytes)
	}
	if cfg.Env != EnvDevelopment || cfg.IsProduction() {
		t.Errorf("APP_ENV = %q, esperaba development", cfg.Env)
	}
	if cfg.DBQueryTimeout != 1500*time.Millisecond || cfg.AdminDBTimeout != 30*time.Second {
		t.Errorf("timeouts = %v y %v", cfg.DBQueryTimeout, cfg.AdminDBTimeout)
	}
	if cfg.RabbitMQPrefetch != 10 || cfg.MaxRetries != 5 {
		t.Errorf("prefetch = %d y reintentos = %d, esperaba 10 y 5", cfg.RabbitMQPrefetch, cfg.MaxRetries)
	}
	if cfg.AdminEnabled() {
		t.Error("sin ADMIN_API_KEY el router de administración no debe arrancar")
	}
}

func TestLoadLeeLosValoresConfigurados(t *testing.T) {
	env := validEnv()
	env["PORT"] = "9000"
	env["HTTP_ADMIN_PORT"] = "9001"
	env["ADMIN_API_KEY"] = strings.Repeat("k", 32)
	env["USER_DELETED_QUEUE"] = "otra.cola.q"
	env["BANNER_MAX_BYTES"] = "1024"
	env["DB_QUERY_TIMEOUT_MS"] = "500"
	env["ADMIN_DB_TIMEOUT_MS"] = "60000"
	env["RABBITMQ_PREFETCH"] = "3"
	env["MAX_RETRIES"] = "0"

	cfg, err := loadFrom(env)
	if err != nil {
		t.Fatalf("error inesperado: %v", err)
	}

	if cfg.Port != "9000" || cfg.AdminPort != "9001" || cfg.UserDeletedQueue != "otra.cola.q" || cfg.BannerMaxBytes != 1024 {
		t.Errorf("configuración leída incorrecta: %+v", cfg)
	}
	if !cfg.AdminEnabled() {
		t.Error("con ADMIN_API_KEY el router de administración debe arrancar")
	}
	if cfg.DBQueryTimeout != 500*time.Millisecond || cfg.AdminDBTimeout != time.Minute || cfg.RabbitMQPrefetch != 3 || cfg.MaxRetries != 0 {
		t.Errorf("valores numéricos incorrectos: %+v", cfg)
	}
}

func TestLoadRechazaLaConfiguracionInvalida(t *testing.T) {
	casos := map[string]struct {
		mutar    func(map[string]string)
		esperado string
	}{
		"sin DATABASE_URL":       {func(e map[string]string) { delete(e, "DATABASE_URL") }, "DATABASE_URL es obligatoria"},
		"sin RABBITMQ_URL":       {func(e map[string]string) { delete(e, "RABBITMQ_URL") }, "RABBITMQ_URL es obligatoria"},
		"sin clave AES":          {func(e map[string]string) { delete(e, "BENEFITS_CODE_AES_KEY") }, "BENEFITS_CODE_AES_KEY es obligatoria"},
		"sin clave HMAC":         {func(e map[string]string) { delete(e, "BENEFITS_CODE_HMAC_KEY") }, "BENEFITS_CODE_HMAC_KEY es obligatoria"},
		"clave AES no es base64": {func(e map[string]string) { e["BENEFITS_CODE_AES_KEY"] = "***" }, "BENEFITS_CODE_AES_KEY debe ser una clave de 32 bytes"},
		"clave HMAC de 31 bytes": {func(e map[string]string) {
			e["BENEFITS_CODE_HMAC_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 31))
		}, "BENEFITS_CODE_HMAC_KEY debe ser una clave de 32 bytes"},
		"claves iguales":                {func(e map[string]string) { e["BENEFITS_CODE_HMAC_KEY"] = e["BENEFITS_CODE_AES_KEY"] }, "independiente"},
		"clave de administración corta": {func(e map[string]string) { e["ADMIN_API_KEY"] = "corta" }, "ADMIN_API_KEY debe tener al menos 32"},
		"puertos iguales":               {func(e map[string]string) { e["PORT"] = "8080"; e["HTTP_ADMIN_PORT"] = "8080" }, "no puede coincidir"},
		"puerto fuera de rango":         {func(e map[string]string) { e["PORT"] = "70000" }, "PORT debe ser un puerto"},
		"puerto no numérico":            {func(e map[string]string) { e["HTTP_ADMIN_PORT"] = "abc" }, "HTTP_ADMIN_PORT debe ser un puerto"},
		"APP_ENV desconocido":           {func(e map[string]string) { e["APP_ENV"] = "staging" }, "APP_ENV debe ser"},
		"timeout no numérico":           {func(e map[string]string) { e["DB_QUERY_TIMEOUT_MS"] = "rápido" }, "DB_QUERY_TIMEOUT_MS inválido"},
		"timeout en cero":               {func(e map[string]string) { e["ADMIN_DB_TIMEOUT_MS"] = "0" }, "ADMIN_DB_TIMEOUT_MS debe estar entre"},
		"prefetch fuera de rango":       {func(e map[string]string) { e["RABBITMQ_PREFETCH"] = "70000" }, "RABBITMQ_PREFETCH debe estar entre"},
		"reintentos negativos":          {func(e map[string]string) { e["MAX_RETRIES"] = "-1" }, "MAX_RETRIES debe estar entre"},
		"banner en cero":                {func(e map[string]string) { e["BANNER_MAX_BYTES"] = "0" }, "BANNER_MAX_BYTES debe estar entre"},
		"banner desmedido":              {func(e map[string]string) { e["BANNER_MAX_BYTES"] = "999999999" }, "BANNER_MAX_BYTES debe estar entre"},
	}

	for nombre, caso := range casos {
		t.Run(nombre, func(t *testing.T) {
			env := validEnv()
			caso.mutar(env)

			cfg, err := loadFrom(env)

			if err == nil || cfg != nil {
				t.Fatalf("esperaba un error, obtuvo cfg=%v err=%v", cfg, err)
			}
			if !strings.Contains(err.Error(), caso.esperado) {
				t.Errorf("error = %q, esperaba que contuviera %q", err, caso.esperado)
			}
		})
	}
}

func TestLoadInformaTodosLosErroresJuntos(t *testing.T) {
	_, err := loadFrom(map[string]string{})
	if err == nil {
		t.Fatal("esperaba un error")
	}
	for _, esperado := range []string{"DATABASE_URL", "RABBITMQ_URL", "BENEFITS_CODE_AES_KEY", "BENEFITS_CODE_HMAC_KEY"} {
		if !strings.Contains(err.Error(), esperado) {
			t.Errorf("el error %q debía mencionar %s", err, esperado)
		}
	}
}

func TestLoadEnProduccionRechazaLosValoresInseguros(t *testing.T) {
	produccion := func() map[string]string {
		env := validEnv()
		env["APP_ENV"] = "production"
		env["DATABASE_URL"] = "postgres://benefits:s3creto@db.interna:5432/benefits?sslmode=require"
		env["RABBITMQ_URL"] = "amqp://benefits:s3creto@mq.interna:5672/"
		return env
	}

	if _, err := loadFrom(produccion()); err != nil {
		t.Fatalf("la configuración de producción válida no debía fallar: %v", err)
	}

	casos := map[string]struct {
		mutar    func(map[string]string)
		esperado string
	}{
		"base sin TLS": {func(e map[string]string) {
			e["DATABASE_URL"] = "postgres://benefits:s3creto@db.interna:5432/benefits?sslmode=disable"
		}, "sslmode=disable"},
		"broker con guest": {func(e map[string]string) { e["RABBITMQ_URL"] = "amqp://guest:guest@mq.interna:5672/" }, "credenciales por defecto"},
		"clave AES de un solo byte": {func(e map[string]string) {
			e["BENEFITS_CODE_AES_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 32))
		}, "BENEFITS_CODE_AES_KEY no puede ser un valor de un solo byte"},
		"clave HMAC de un solo byte": {func(e map[string]string) {
			e["BENEFITS_CODE_HMAC_KEY"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
		}, "BENEFITS_CODE_HMAC_KEY no puede ser un valor de un solo byte"},
	}
	for nombre, caso := range casos {
		t.Run(nombre, func(t *testing.T) {
			env := produccion()
			caso.mutar(env)

			_, err := loadFrom(env)

			if err == nil || !strings.Contains(err.Error(), caso.esperado) {
				t.Fatalf("error = %v, esperaba que contuviera %q", err, caso.esperado)
			}
		})
	}
}

func TestEnDesarrolloLosValoresComodosSeAceptan(t *testing.T) {
	// Es la configuración del docker-compose de desarrollo: sslmode=disable y guest.
	if _, err := loadFrom(validEnv()); err != nil {
		t.Fatalf("la configuración de desarrollo no debía fallar: %v", err)
	}
}

func TestLosErroresDeConfiguracionNoReflejanLosSecretos(t *testing.T) {
	env := validEnv()
	env["BENEFITS_CODE_AES_KEY"] = "SECRETO-NO-BASE64***"
	env["ADMIN_API_KEY"] = "SECRETO-CORTO"

	_, err := loadFrom(env)

	if err == nil || strings.Contains(err.Error(), "SECRETO") {
		t.Fatalf("el error refleja un secreto: %v", err)
	}
}

func TestSecretNuncaSeImprime(t *testing.T) {
	secreto := Secret("valor-secreto")

	salidas := []string{
		secreto.String(),
		fmt.Sprintf("%v", secreto),
		//lint:ignore S1025 se prueba a propósito el verbo %s de fmt
		fmt.Sprintf("%s", secreto),
		fmt.Sprintf("%#v", secreto),
		fmt.Sprintf("%+v", struct{ K Secret }{secreto}),
		secreto.LogValue().String(),
		slog.AnyValue(secreto).Resolve().String(),
	}
	for _, salida := range salidas {
		if strings.Contains(salida, "valor-secreto") {
			t.Errorf("la salida %q filtra el secreto", salida)
		}
	}
	if secreto.Reveal() != "valor-secreto" {
		t.Error("Reveal debe devolver el valor real")
	}
}

func TestLoadLeeElEntornoDelProceso(t *testing.T) {
	for nombre, valor := range validEnv() {
		t.Setenv(nombre, valor)
	}
	t.Setenv("PORT", "8123")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("error inesperado: %v", err)
	}
	if cfg.Port != "8123" {
		t.Errorf("PORT = %q, esperaba 8123", cfg.Port)
	}
}

func TestLoadSinEntornoFalla(t *testing.T) {
	for _, nombre := range []string{"DATABASE_URL", "RABBITMQ_URL", "BENEFITS_CODE_AES_KEY", "BENEFITS_CODE_HMAC_KEY"} {
		t.Setenv(nombre, "")
	}
	if _, err := Load(); err == nil {
		t.Fatal("esperaba un error sin variables obligatorias")
	}
}

func TestSslModeDisabledConUnaCadenaIlegibleNoRechazaPorSiSola(t *testing.T) {
	if sslModeDisabled("://no es una url") {
		t.Error("una cadena ilegible no declara sslmode=disable")
	}
	if !sslModeDisabled("postgres://u:p@h/db?sslmode=disable") {
		t.Error("sslmode=disable debía detectarse")
	}
	if sslModeDisabled("postgres://u:p@h/db?sslmode=verify-full") {
		t.Error("verify-full no es sslmode=disable")
	}
}

func TestIsRepeatedByte(t *testing.T) {
	if isRepeatedByte(nil) {
		t.Error("una clave ausente no es un byte repetido: el error ya se informó aparte")
	}
	if !isRepeatedByte([]byte{7, 7, 7}) || isRepeatedByte([]byte{7, 7, 8}) {
		t.Error("isRepeatedByte decide mal")
	}
}
