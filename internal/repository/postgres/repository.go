package postgres

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

const (
	// defaultQueryTimeout acota cada consulta de los endpoints públicos y del consumidor
	// cuando el llamador no fija uno con WithQueryTimeout. Alinea con DB_QUERY_TIMEOUT_MS.
	defaultQueryTimeout = 1500 * time.Millisecond
	// defaultAdminTimeout acota cada operación de administración, que puede mover un lote
	// de hasta 100.000 códigos. Alinea con ADMIN_DB_TIMEOUT_MS.
	defaultAdminTimeout = 30 * time.Second

	// batchSize es cuántas filas se mandan por sentencia en las operaciones masivas. Con
	// pocas columnas por fila queda muy por debajo del tope de 65535 parámetros de
	// PostgreSQL.
	batchSize = 1000

	// Estados de los códigos, tal como se guardan.
	statusAvailable = string(domain.CodeAvailable)
	statusAssigned  = string(domain.CodeAssigned)
	statusBurned    = string(domain.CodeBurned)
)

// classifyError distingue una indisponibilidad de infraestructura (la base es
// inalcanzable o la consulta excedió su plazo) de un error propio de la aplicación.
// Solo la primera categoría se envuelve en domain.ErrRepositoryUnavailable, que la capa
// HTTP mapea a 503.
func classifyError(err error) error {
	if err == nil {
		return nil
	}

	var domainErr *domainError
	if errors.As(err, &domainErr) {
		return domainErr.err
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.Join(domain.ErrRepositoryUnavailable, err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return errors.Join(domain.ErrRepositoryUnavailable, err)
	}

	// SQLSTATE de indisponibilidad: 08xxx (conexión), 53xxx (recursos), 57014
	// (statement_timeout) y 57P0x (reinicio o apagado).
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		code := pgErr.Code
		if len(code) >= 2 && (code[:2] == "08" || code[:2] == "53" || code == "57014" || (len(code) >= 4 && code[:4] == "57P0")) {
			return errors.Join(domain.ErrRepositoryUnavailable, err)
		}
	}

	return err
}

// domainError envuelve un error de dominio devuelto dentro de una transacción, para
// que GORM haga rollback y classifyError lo devuelva intacto.
type domainError struct{ err error }

func (e *domainError) Error() string { return e.err.Error() }
func (e *domainError) Unwrap() error { return e.err }

// Repository implementa domain.BenefitRepository y domain.CampaignRepository sobre
// PostgreSQL vía GORM.
type Repository struct {
	db           *gorm.DB
	queryTimeout time.Duration
	adminTimeout time.Duration
}

var (
	_ domain.BenefitRepository  = (*Repository)(nil)
	_ domain.CampaignRepository = (*Repository)(nil)
)

// Option configura aspectos opcionales de Repository.
type Option func(*Repository)

// WithQueryTimeout fija el plazo máximo de cada consulta pública. Agotado, el contexto
// se cancela y classifyError lo reclasifica como domain.ErrRepositoryUnavailable.
func WithQueryTimeout(timeout time.Duration) Option {
	return func(r *Repository) { r.queryTimeout = timeout }
}

// WithAdminTimeout fija el plazo máximo de cada operación de administración.
func WithAdminTimeout(timeout time.Duration) Option {
	return func(r *Repository) { r.adminTimeout = timeout }
}

// NewRepository construye el repositorio sobre la conexión dada.
func NewRepository(db *gorm.DB, opts ...Option) *Repository {
	r := &Repository{db: db, queryTimeout: defaultQueryTimeout, adminTimeout: defaultAdminTimeout}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Ping verifica la conectividad con la base de datos.
func (r *Repository) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	sqlDB, err := r.db.DB()
	if err != nil {
		return classifyError(err)
	}
	return classifyError(sqlDB.PingContext(ctx))
}
