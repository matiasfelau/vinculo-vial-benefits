// Package pgtest aísla las pruebas que usan PostgreSQL: cada prueba trabaja en su propio
// esquema, así los paquetes pueden correr en paralelo contra la misma base sin pisarse.
// Solo lo usan las pruebas.
package pgtest

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	// Registra el driver "pgx" de database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	// EnvDatabaseURL es la variable con la base de pruebas. Sin ella, las pruebas que la
	// necesitan se omiten: no se simula PostgreSQL con otro motor porque el comportamiento
	// bajo prueba (locks, índices parciales, SQLSTATE) es el del motor real.
	EnvDatabaseURL = "DATABASE_URL"

	setupTimeout = 10 * time.Second
)

// NewSchemaDSN crea un esquema nuevo, lo borra al terminar la prueba y devuelve una
// cadena de conexión cuyo search_path apunta a él. Omite la prueba si no hay base.
func NewSchemaDSN(t *testing.T) string {
	t.Helper()

	base := os.Getenv(EnvDatabaseURL)
	if base == "" {
		t.Skip("DATABASE_URL no configurada: se omite la prueba de integración con PostgreSQL")
	}

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("no se pudo abrir la base de pruebas: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	schema := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("no se pudo crear el esquema de la prueba: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), setupTimeout)
		defer dropCancel()
		_, _ = admin.ExecContext(dropCtx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		_ = admin.Close()
	})

	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("DATABASE_URL ilegible: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
