// Package config centraliza la carga de configuración del servicio desde variables de entorno.
//
// La configuración obligatoria se valida al arrancar: ante una ausencia o un valor
// inválido, Load devuelve error y el proceso termina con código distinto de cero. No
// hay valores por defecto silenciosos para los secretos.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvDevelopment y EnvProduction son los valores de APP_ENV con significado propio.
	EnvDevelopment = "development"
	EnvProduction  = "production"

	defaultPort      = "8080"
	defaultAdminPort = "8082"

	defaultUserDeletedQueue = "benefits.user-deleted.q"

	// defaultBannerMaxBytes es el tope del banner: 300 KiB.
	defaultBannerMaxBytes = 307200
	// maxBannerMaxBytes acota BANNER_MAX_BYTES: el banner vive en una columna bytea de
	// la fila de la campaña, así que un tope desmedido inflaría la base.
	maxBannerMaxBytes = 10 << 20

	// minAdminAPIKeyLength es el piso de ADMIN_API_KEY: es un secreto compartido
	// comparado byte a byte, así que su largo es toda la defensa contra adivinarlo.
	minAdminAPIKeyLength = 32

	// codeKeyLength es el largo en bytes de las claves de los códigos (AES-256 y HMAC-SHA256).
	codeKeyLength = 32

	// maxRabbitMQPrefetch es el mayor prefetch que admite basic.qos: el campo del
	// protocolo es de 16 bits y un valor mayor se truncaría en silencio.
	maxRabbitMQPrefetch = 65535

	defaultDBQueryTimeoutMS      = "1500"
	defaultAdminDBTimeoutMS      = "30000"
	defaultRabbitMQPublishTOMS   = "2000"
	defaultRabbitMQPrefetch      = "10"
	defaultMaxRetries            = "5"
	redactedSecret               = "[REDACTED]"
	insecureRabbitMQCredentials  = "guest:guest@"
	insecureDatabaseSSLModeValue = "disable"
)

// Secret es un valor que nunca debe aparecer en un log ni en un mensaje de error: su
// representación textual está redactada. Reveal devuelve el valor real.
type Secret string

// Reveal devuelve el valor real del secreto.
func (s Secret) Reveal() string { return string(s) }

// String redacta el valor, para que un %v o %s accidental no lo filtre.
func (s Secret) String() string { return redactedSecret }

// GoString redacta el valor en %#v.
func (s Secret) GoString() string { return redactedSecret }

// LogValue redacta el valor en los logs estructurados.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redactedSecret) }

// Config agrupa los parámetros de configuración del servicio.
type Config struct {
	// Env es APP_ENV: "development" (por defecto) o "production", que endurece la validación.
	Env string

	// Port es el puerto del router público (PORT). Kong lo alcanza acá.
	Port string
	// AdminPort es el puerto del router de administración (HTTP_ADMIN_PORT). Kong no lo
	// expone.
	AdminPort string
	// AdminAPIKey es la clave de X-Admin-Api-Key (ADMIN_API_KEY). Vacía, el router de
	// administración no arranca.
	AdminAPIKey Secret

	// DatabaseURL es la cadena de conexión a la base propia.
	DatabaseURL Secret
	// DBQueryTimeout acota cada consulta de los endpoints públicos y del consumidor.
	DBQueryTimeout time.Duration
	// AdminDBTimeout acota cada operación del router de administración. Es mayor que
	// DBQueryTimeout porque la carga de un lote de hasta 100.000 códigos lo requiere.
	AdminDBTimeout time.Duration

	// RabbitMQURL es la URL del broker compartido.
	RabbitMQURL Secret
	// RabbitMQPublishTimeout acota la publicación a la DLQ, incluida la espera del confirm.
	RabbitMQPublishTimeout time.Duration
	// UserDeletedQueue es la cola de la que se consume user.deleted. La topología (la
	// cola, su .retry y su .dlq) la declara solo vinculo-vial-core.
	UserDeletedQueue string
	// RabbitMQPrefetch es el basic.qos del consumidor.
	RabbitMQPrefetch int
	// MaxRetries es cuántas veces se reintenta un mensaje con fallo transitorio antes de
	// derivarlo a la DLQ.
	MaxRetries int64

	// CodeAESKey y CodeHMACKey son las claves (32 bytes en base64) con las que se cifra
	// cada código y se calcula su HMAC. Son independientes entre sí.
	CodeAESKey  Secret
	CodeHMACKey Secret

	// BannerMaxBytes es el tamaño máximo del banner de una campaña.
	BannerMaxBytes int64
}

// IsProduction informa si el servicio corre con la configuración endurecida.
func (c *Config) IsProduction() bool {
	return strings.EqualFold(c.Env, EnvProduction)
}

// AdminEnabled informa si el router de administración debe arrancar.
func (c *Config) AdminEnabled() bool {
	return c.AdminAPIKey.Reveal() != ""
}

// Load construye la configuración a partir de las variables de entorno del proceso.
func Load() (*Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom es Load sobre una función de lectura inyectable, para usarlo y probarlo sin
// tocar el entorno del proceso.
func LoadFrom(getenv func(string) string) (*Config, error) {
	r := &reader{getenv: getenv}

	cfg := &Config{
		Env:              r.str("APP_ENV", EnvDevelopment),
		Port:             r.str("PORT", defaultPort),
		AdminPort:        r.str("HTTP_ADMIN_PORT", defaultAdminPort),
		AdminAPIKey:      Secret(r.str("ADMIN_API_KEY", "")),
		DatabaseURL:      Secret(r.str("DATABASE_URL", "")),
		RabbitMQURL:      Secret(r.str("RABBITMQ_URL", "")),
		UserDeletedQueue: r.str("USER_DELETED_QUEUE", defaultUserDeletedQueue),
		CodeAESKey:       Secret(r.str("BENEFITS_CODE_AES_KEY", "")),
		CodeHMACKey:      Secret(r.str("BENEFITS_CODE_HMAC_KEY", "")),
	}

	cfg.DBQueryTimeout = r.millis("DB_QUERY_TIMEOUT_MS", defaultDBQueryTimeoutMS)
	cfg.AdminDBTimeout = r.millis("ADMIN_DB_TIMEOUT_MS", defaultAdminDBTimeoutMS)
	cfg.RabbitMQPublishTimeout = r.millis("RABBITMQ_PUBLISH_TIMEOUT_MS", defaultRabbitMQPublishTOMS)
	cfg.RabbitMQPrefetch = int(r.intInRange("RABBITMQ_PREFETCH", defaultRabbitMQPrefetch, 1, maxRabbitMQPrefetch))
	cfg.MaxRetries = r.intInRange("MAX_RETRIES", defaultMaxRetries, 0, math.MaxInt32)
	cfg.BannerMaxBytes = r.intInRange("BANNER_MAX_BYTES", strconv.Itoa(defaultBannerMaxBytes), 1, maxBannerMaxBytes)

	cfg.validate(r)

	if err := errors.Join(r.errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate aplica las reglas que cruzan variables o dependen del entorno.
func (c *Config) validate(r *reader) {
	switch {
	case !strings.EqualFold(c.Env, EnvDevelopment) && !c.IsProduction():
		r.fail("config: APP_ENV debe ser %q o %q", EnvDevelopment, EnvProduction)
	case c.Port == c.AdminPort:
		r.fail("config: HTTP_ADMIN_PORT no puede coincidir con PORT: el router de administración perdería su puerto propio")
	}
	r.port("PORT", c.Port)
	r.port("HTTP_ADMIN_PORT", c.AdminPort)

	r.required("DATABASE_URL", c.DatabaseURL.Reveal())
	r.required("RABBITMQ_URL", c.RabbitMQURL.Reveal())
	r.required("USER_DELETED_QUEUE", c.UserDeletedQueue)

	if key := c.AdminAPIKey.Reveal(); key != "" && len(key) < minAdminAPIKeyLength {
		r.fail("config: ADMIN_API_KEY debe tener al menos %d caracteres", minAdminAPIKeyLength)
	}

	aesKey := r.key("BENEFITS_CODE_AES_KEY", c.CodeAESKey.Reveal())
	hmacKey := r.key("BENEFITS_CODE_HMAC_KEY", c.CodeHMACKey.Reveal())
	if aesKey != nil && hmacKey != nil && string(aesKey) == string(hmacKey) {
		r.fail("config: BENEFITS_CODE_HMAC_KEY debe ser independiente de BENEFITS_CODE_AES_KEY")
	}

	if c.IsProduction() {
		c.validateProduction(r, aesKey, hmacKey)
	}
}

// validateProduction rechaza los valores inseguros que en desarrollo son cómodos.
func (c *Config) validateProduction(r *reader, aesKey, hmacKey []byte) {
	if sslModeDisabled(c.DatabaseURL.Reveal()) {
		r.fail("config: DATABASE_URL con sslmode=disable no es admisible con APP_ENV=production")
	}
	if strings.Contains(c.RabbitMQURL.Reveal(), insecureRabbitMQCredentials) {
		r.fail("config: RABBITMQ_URL con las credenciales por defecto (guest) no es admisible con APP_ENV=production")
	}
	if isRepeatedByte(aesKey) {
		r.fail("config: BENEFITS_CODE_AES_KEY no puede ser un valor de un solo byte repetido con APP_ENV=production")
	}
	if isRepeatedByte(hmacKey) {
		r.fail("config: BENEFITS_CODE_HMAC_KEY no puede ser un valor de un solo byte repetido con APP_ENV=production")
	}
}

// sslModeDisabled informa si la cadena de conexión fija sslmode=disable.
func sslModeDisabled(dsn string) bool {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	return parsed.Query().Get("sslmode") == insecureDatabaseSSLModeValue
}

// isRepeatedByte informa si todos los bytes de key son iguales: una clave de ese
// estilo (por ejemplo, 32 ceros) es un marcador de posición, no una clave.
func isRepeatedByte(key []byte) bool {
	if len(key) == 0 {
		return false
	}
	for _, b := range key {
		if b != key[0] {
			return false
		}
	}
	return true
}

// reader acumula los errores de lectura para informarlos todos juntos.
type reader struct {
	getenv func(string) string
	errs   []error
}

func (r *reader) fail(format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf(format, args...))
}

func (r *reader) str(name, fallback string) string {
	if v := strings.TrimSpace(r.getenv(name)); v != "" {
		return v
	}
	return fallback
}

func (r *reader) required(name, value string) {
	if value == "" {
		r.fail("config: %s es obligatoria", name)
	}
}

func (r *reader) port(name, value string) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 65535 {
		r.fail("config: %s debe ser un puerto entre 1 y 65535", name)
	}
}

// intInRange lee name como un entero dentro de [lo, hi]. Ante un error lo acumula y
// devuelve lo.
func (r *reader) intInRange(name, fallback string, lo, hi int64) int64 {
	value, err := strconv.ParseInt(r.str(name, fallback), 10, 64)
	if err != nil {
		r.fail("config: %s inválido: debe ser un entero", name)
		return lo
	}
	if value < lo || value > hi {
		r.fail("config: %s debe estar entre %d y %d", name, lo, hi)
		return lo
	}
	return value
}

// millis lee name como una cantidad positiva de milisegundos.
func (r *reader) millis(name, fallback string) time.Duration {
	ms := r.intInRange(name, fallback, 1, math.MaxInt32)
	return time.Duration(ms) * time.Millisecond
}

// key valida que value sea una clave de 32 bytes en base64 y devuelve sus bytes, o nil
// si falta o es inválida (el error ya quedó acumulado). Nunca incluye el valor en el
// mensaje.
func (r *reader) key(name, value string) []byte {
	if value == "" {
		r.fail("config: %s es obligatoria", name)
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) != codeKeyLength {
		r.fail("config: %s debe ser una clave de %d bytes codificada en base64", name, codeKeyLength)
		return nil
	}
	return raw
}
