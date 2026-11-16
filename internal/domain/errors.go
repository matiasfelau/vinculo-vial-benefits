// Package domain define las entidades, las reglas y los puertos del servicio de
// beneficios. No depende de ningún framework ni de la base de datos.
package domain

import "errors"

var (
	// ErrBenefitNotFound indica que la campaña no existe o no está vigente. El cliente
	// no distingue un caso del otro.
	ErrBenefitNotFound = errors.New("domain: la campaña no existe o no está vigente")

	// ErrNoCodesLeft indica que la campaña vigente ya no tiene códigos disponibles.
	ErrNoCodesLeft = errors.New("domain: la campaña no tiene códigos disponibles")

	// ErrCampaignNotFound indica que la campaña no existe (rutas de administración).
	ErrCampaignNotFound = errors.New("domain: la campaña no existe")

	// ErrInvalidCampaign indica que los datos de la campaña no cumplen el contrato. Su
	// mensaje describe el campo, nunca su valor.
	ErrInvalidCampaign = errors.New("domain: datos de campaña inválidos")

	// ErrInvalidBanner indica que el banner no es PNG, JPEG ni WebP según sus bytes.
	ErrInvalidBanner = errors.New("domain: el banner no es una imagen PNG, JPEG ni WebP")

	// ErrBannerTooLarge indica que el banner supera BANNER_MAX_BYTES.
	ErrBannerTooLarge = errors.New("domain: el banner supera el tamaño máximo")

	// ErrTooManyLines indica que un CSV supera el máximo de líneas.
	ErrTooManyLines = errors.New("domain: el CSV supera el máximo de líneas")

	// ErrRepositoryUnavailable indica que la base de datos es inalcanzable o que una
	// consulta excedió su plazo. Se mapea a 503: es una indisponibilidad de
	// infraestructura, no un defecto del servicio.
	ErrRepositoryUnavailable = errors.New("domain: no se pudo consultar la base de datos")
)
