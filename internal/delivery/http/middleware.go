// Package http contiene los handlers HTTP del servicio (capa de entrega): el router
// público, que alcanza Kong, y el de administración, en un puerto propio.
package http

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// HeaderRequestID es la cabecera de correlación que propaga el gateway.
	HeaderRequestID = "X-Request-ID"
	// HeaderUserID la inyecta Kong a partir del JWT (plugin token-offloading). El servicio
	// nunca la acepta por otra vía: Kong descarta cualquier X-User-* que traiga el cliente.
	HeaderUserID = "X-User-Id"
	// HeaderAdminAPIKey es la cabecera donde el operador envía la clave de administración.
	HeaderAdminAPIKey = "X-Admin-Api-Key"

	contextRequestID = "request_id"
	contextUserID    = "user_id"

	// maxRequestIDLength acota el X-Request-ID que se acepta del cliente.
	maxRequestIDLength = 128
	// maxLoggedPathLength acota lo que se registra del path pedido.
	maxLoggedPathLength = 256
)

// requestID reutiliza el identificador que ya trae la petición desde Kong y solo
// genera uno nuevo si falta, para que una traza atraviese gateway y servicio.
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(HeaderRequestID)
		if id == "" || len(id) > maxRequestIDLength {
			id = uuid.NewString()
		}
		c.Set(contextRequestID, id)
		c.Writer.Header().Set(HeaderRequestID, id)
		c.Next()
	}
}

func requestIDOf(c *gin.Context) string {
	if id, ok := c.Get(contextRequestID); ok {
		if s, ok := id.(string); ok {
			return s
		}
	}
	return ""
}

// requestPath devuelve la ruta pedida, acotada y sin la query string.
func requestPath(c *gin.Context) string {
	path := c.Request.URL.Path
	if len(path) > maxLoggedPathLength {
		return path[:maxLoggedPathLength] + "…"
	}
	return path
}

// accessLog registra una línea estructurada por petición. No toca el cuerpo ni los
// encabezados de identidad: un código jamás pasa por el log.
func accessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		level := slog.LevelInfo
		if c.Writer.Status() >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		logger.LogAttrs(c.Request.Context(), level, "petición http",
			slog.String("request_id", requestIDOf(c)),
			slog.String("method", c.Request.Method),
			slog.String("path", requestPath(c)),
			slog.String("route", c.FullPath()),
			slog.Int("status", c.Writer.Status()),
			slog.Duration("latency", time.Since(start)),
		)
	}
}

// recovery convierte un panic en un 500 genérico y deja el stack en el log, nunca en la
// respuesta.
func recovery(logger *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, recovered any) {
		logger.ErrorContext(c.Request.Context(), "panic atendiendo la petición",
			slog.String("request_id", requestIDOf(c)),
			slog.String("path", requestPath(c)),
			slog.String("route", c.FullPath()),
			slog.Any("panic", recovered),
		)
		c.AbortWithStatusJSON(http.StatusInternalServerError, errorBody{Error: "error interno", Code: codeInternal})
	})
}

// securityHeaders evita que un navegador reinterprete el tipo de una respuesta.
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("X-Content-Type-Options", "nosniff")
		c.Next()
	}
}

// noStore evita que una respuesta con un código quede cacheada en un intermediario.
func noStore() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Cache-Control", "no-store")
		c.Next()
	}
}

// requireUser exige el X-User-Id que inyecta Kong. Sin un UUID válido responde 401: el
// servicio deniega por defecto y no depende de que Kong haya filtrado antes.
func requireUser(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, err := uuid.Parse(c.GetHeader(HeaderUserID))
		if err != nil || userID == uuid.Nil {
			logger.WarnContext(c.Request.Context(), "petición sin identidad de usuario válida",
				slog.String("request_id", requestIDOf(c)),
				slog.String("path", requestPath(c)),
			)
			c.AbortWithStatusJSON(http.StatusUnauthorized, errorBody{Error: "no autorizado", Code: codeUnauthorized})
			return
		}
		c.Set(contextUserID, userID)
		c.Next()
	}
}

func userIDOf(c *gin.Context) uuid.UUID {
	if id, ok := c.Get(contextUserID); ok {
		if userID, ok := id.(uuid.UUID); ok {
			return userID
		}
	}
	return uuid.Nil
}

// requireAdminAPIKey exige, en toda petición, la clave de administración.
//
// Se aplica antes de que el router resuelva si la ruta existe: sin la clave correcta el
// status es 401 tanto si el path es real como si no, así que nunca delata qué rutas
// expone este servidor. La comparación es a tiempo constante y sobre los SHA-256 de ambos
// valores: ni el contenido ni el largo de la clave real influyen en el tiempo de
// respuesta.
func requireAdminAPIKey(key string, logger *slog.Logger) gin.HandlerFunc {
	want := sha256.Sum256([]byte(key))
	return func(c *gin.Context) {
		got := sha256.Sum256([]byte(c.GetHeader(HeaderAdminAPIKey)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			logger.WarnContext(c.Request.Context(), "petición a una ruta de administración sin clave válida",
				slog.String("request_id", requestIDOf(c)),
				slog.String("path", requestPath(c)),
			)
			c.AbortWithStatusJSON(http.StatusUnauthorized, errorBody{Error: "no autorizado", Code: codeUnauthorized})
			return
		}
		c.Next()
	}
}
