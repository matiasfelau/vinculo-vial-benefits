# vinculo-vial-benefits

Servicio de beneficios de Vínculo Vial. Un partner (por ejemplo, una petrolera) entrega un lote de códigos de descuento de un solo uso por campaña; el servicio asigna **uno por persona y por campaña** a cualquier usuario registrado, y el usuario lo canjea en la app del partner.

Vínculo Vial **no genera ni valida** los códigos: solo los distribuye. El partner devuelve un reporte de canjes para medir la campaña, y **nunca recibe datos de los usuarios**, solo estadísticas agregadas.

Stack: Go, Gin, GORM con PostgreSQL y RabbitMQ (la misma base que `vinculo-vial-vehicle`).

## Reglas

- **Un código por persona y por campaña.** `claim` es idempotente: pedirlo dos veces devuelve el mismo código.
- **Nunca el mismo código a dos personas**, ni con pedidos concurrentes. Todo ocurre en una transacción: un lock de aviso por (campaña, usuario) serializa los pedidos de una misma persona; entre personas distintas, `FOR UPDATE SKIP LOCKED` hace que cada una tome un código distinto sin bloquearse. Dos índices únicos de la base son la última defensa.
- **Campaña vigente:** `active` y la hora actual entre `starts_at` (inclusive) y `ends_at` (exclusive).
- **Campaña visible en la lista:** vigente y con códigos `AVAILABLE`, o con un código ya asignado al usuario. Una campaña agotada desaparece de la lista de quien no tiene código, pero quien ya lo tiene lo sigue viendo y puede volver a pedirlo.
- **Baja de cuenta (`user.deleted`):** los códigos del usuario pasan a `BURNED` y su `assigned_to` queda en `null`. No vuelven al pool, porque el usuario pudo haberlos visto o canjeado. El consumo es idempotente.
- **Banners:** viven en la base (columna `bytea`), los sirve este servicio y su tipo se detecta por los bytes (PNG, JPEG o WebP), nunca por la extensión ni por el `Content-Type` del cliente.
- **Logs:** nunca registran códigos, ni en claro ni cifrados.

## Estructura

```
cmd/api                       arranque (run) y apagado ordenado
cmd/seed                      datos de prueba (solo APP_ENV=development)
internal/domain               entidades, reglas y puertos
internal/service              benefit (público), campaign (administración), userdeleted
internal/delivery/http        router público, router de administración, middlewares
internal/delivery/events      consumidor de user.deleted (reintentos y DLQ)
internal/repository/postgres  GORM: modelos, migración, claim transaccional
internal/crypto/codes         AES-256-GCM y HMAC-SHA256 de los códigos
internal/banner               validación de imágenes por firma de bytes
internal/seed, internal/lifecycle, internal/testutil/pgtest, pkg/config
```

## Endpoints públicos

Los alcanza Kong con `jwt` y `token-offloading`, que inyecta `X-User-Id`. El servicio exige ese encabezado (un UUID) en cada ruta de `/api/v1` y responde `401 UNAUTHORIZED` si falta o es inválido: no depende de que Kong lo haya filtrado antes.

| Método y ruta | Respuesta |
|---|---|
| `GET /api/v1/benefits` | `200 [ { id, partner_name, title, description, terms, open_url, ends_at, banner_url, code } ]` ordenadas por `ends_at`. `banner_url` es `/api/v1/benefits/{id}/banner`. `code` es `null` si el usuario todavía no lo pidió |
| `GET /api/v1/benefits/{id}/banner` | La imagen, con `Content-Type`, `ETag` (SHA-256 entre comillas) y `Cache-Control: private, max-age=3600`. Con un `If-None-Match` que coincide, `304` sin cuerpo. `404 BENEFIT_NOT_FOUND` |
| `POST /api/v1/benefits/{id}/claim` | `200 { "code" }`. `404 BENEFIT_NOT_FOUND` si no existe o no está vigente; `409 NO_CODES_LEFT` si el pool se agotó |

Además, sin identidad: `GET /health`, `GET /ready` (base de datos) y `GET /health/deps` (consumidor de RabbitMQ; `503` si está caído).

**Errores:** `{ "error", "code" }`. Los `503` llevan `DEPENDENCY_UNAVAILABLE` y `Retry-After: 5`; los `500`, `INTERNAL_ERROR`, sin detalles internos. Las respuestas con códigos llevan `Cache-Control: no-store`.

## Endpoints de administración

Van en un `*http.Server` aparte, en el puerto `HTTP_ADMIN_PORT` (8082), con `X-Admin-Api-Key`. **Kong no los expone** y no existen en el router público. En el `docker-compose.yml` de desarrollo se publican solo en `127.0.0.1:8084`.

- Toda ruta exige `X-Admin-Api-Key` igual a `ADMIN_API_KEY` (mínimo 32 caracteres), comparada a tiempo constante. Sin clave válida, `401` en cualquier ruta, exista o no.
- Si `ADMIN_API_KEY` está vacía, el router de administración **no arranca** y el servicio loguea un aviso.
- El valor debe ser distinto en cada servicio.

| Método y ruta | Body | Respuesta |
|---|---|---|
| `GET /admin/v1/campaigns` | — | `200 { "items": [ { id, partner_name, title, description, terms, open_url, starts_at, ends_at, active, created_at, updated_at, stats } ] }`, de la más nueva a la más vieja |
| `POST /admin/v1/campaigns` | Multipart: parte `campaign` (JSON) y parte `banner` | `201 { "id" }` |
| `PATCH /admin/v1/campaigns/{id}` | JSON parcial: `title`, `description`, `terms`, `open_url`, `starts_at`, `ends_at`, `active` | `204` |
| `PUT /admin/v1/campaigns/{id}/banner` | La imagen | `204` |
| `POST /admin/v1/campaigns/{id}/codes` | `text/csv`: un código por línea | `200 { "inserted", "duplicates", "invalid" }` |
| `POST /admin/v1/campaigns/{id}/redemptions` | `text/csv`: los códigos canjeados que reporta el partner | `200 { "matched", "unknown" }` |
| `GET /admin/v1/campaigns/{id}/stats` | — | `200 { "available", "assigned", "burned", "redeemed" }` |

Detalles:

- **Alta:** la parte `campaign` lleva `partner_name` (hasta 80), `title` (80), `description` (500), `terms` (2000), `open_url` (solo `https://`, hasta 500, sin credenciales), `starts_at` y `ends_at` (RFC 3339, `ends_at > starts_at`) y, opcional, `active` (por defecto `false`). `campaign` puede ir como campo o como archivo del multipart. `partner_name` no se puede cambiar por `PATCH`.
- **Códigos:** de 4 a 64 caracteres `[A-Za-z0-9-]`, hasta 100.000 líneas. Las líneas en blanco se ignoran. `invalid` cuenta las líneas con formato inválido; `duplicates`, las repetidas en el archivo y las que ya estaban cargadas. La carga es todo o nada. Los códigos distinguen mayúsculas.
- **Canjes:** `matched` cuenta los códigos de la campaña que figuran en el reporte (aunque ya estuvieran canjeados: reportar dos veces es inocuo y `redeemed_at` conserva la primera fecha); `unknown`, los que no existen en la campaña y las líneas con formato inválido. Un código repetido en el reporte cuenta una vez. También se marca un código `BURNED`: su dueño pudo canjearlo antes de la baja.
- **Estadísticas:** `available`, `assigned` y `burned` cuentan por estado; `redeemed`, los códigos con `redeemed_at`.
- **Límites:** banner hasta `BANNER_MAX_BYTES` (`413 BANNER_TOO_LARGE`), CSV hasta 6,6 MB (`413 PAYLOAD_TOO_LARGE`), más de 100.000 líneas (`400 TOO_MANY_LINES`).
- **Errores propios:** `404 CAMPAIGN_NOT_FOUND`, `400 INVALID_BODY` (con el campo que falla, nunca su valor), `400 INVALID_BANNER`, `415 UNSUPPORTED_MEDIA_TYPE` (los CSV exigen `Content-Type: text/csv`).

Ejemplos (con `ADMIN=http://127.0.0.1:8084` y la clave en `KEY`):

```bash
# Alta con banner
curl -H "X-Admin-Api-Key: $KEY" \
  -F "campaign=@campaign.json;type=application/json" -F "banner=@banner.png;type=image/png" \
  $ADMIN/admin/v1/campaigns

# Lote de códigos y reporte de canjes
curl -H "X-Admin-Api-Key: $KEY" -H "Content-Type: text/csv" --data-binary @codigos.csv  $ADMIN/admin/v1/campaigns/$ID/codes
curl -H "X-Admin-Api-Key: $KEY" -H "Content-Type: text/csv" --data-binary @canjes.csv   $ADMIN/admin/v1/campaigns/$ID/redemptions
curl -H "X-Admin-Api-Key: $KEY" $ADMIN/admin/v1/campaigns/$ID/stats
```

## Eventos

Benefits consume y no publica, así que no usa outbox.

| Evento | Cola | Qué hace |
|---|---|---|
| `user.deleted` | `benefits.user-deleted.q` | Pasa a `BURNED` los códigos del usuario (el `actor` del evento) |

La topología (la cola, su `.retry` y su `.dlq`) la declara solo `vinculo-vial-core`.

- **Idempotencia:** repetir el evento no encuentra códigos asignados que quemar, así que el resultado no cambia, con cualquier `eventId`.
- **Clasificación:** un envelope o payload inválido (sin `actor` UUID o sin `deleted_at` RFC 3339), o un panic del procesador, va directo a la DLQ. Una falla de la base se reintenta por `.retry` (con `x-death`) hasta `MAX_RETRIES` y después va a la DLQ. El `ack` es siempre manual, tras confirmar la persistencia, y la derivación a la DLQ espera el publisher confirm.
- **Sobre el `actor`:** en `user.deleted` el `actor` es el usuario dado de baja, así que sin él no hay nada que hacer y el evento es inválido (igual que en Vehicle y Alert). La regla «un evento sin `actor` no va a la DLQ por eso» vale para los eventos que salen sin `actor` por diseño, que Benefits no consume.
- Si el broker está caído, el servicio arranca igual, sirve el HTTP y el consumidor reconecta con backoff exponencial (`/health/deps` informa `503`).

## Datos

PostgreSQL propio. El esquema lo crea `AutoMigrate` al arrancar, como en Vehicle.

- **`campaigns`:** `id`, `partner_name`, `title`, `description`, `terms`, `open_url`, `starts_at`, `ends_at` (con `CHECK ends_at > starts_at`), `active` (por defecto `false`), `banner` (`bytea`), `banner_content_type`, `banner_etag` (SHA-256 del banner), `created_at`, `updated_at`.
- **`codes`:** `id`, `campaign_id` (FK), `code_enc` (AES-256-GCM con `BENEFITS_CODE_AES_KEY`), `code_hmac` (HMAC-SHA256 con `BENEFITS_CODE_HMAC_KEY`), `status` (`AVAILABLE`, `ASSIGNED` o `BURNED`, con `CHECK`), `assigned_to`, `assigned_at`, `redeemed_at`. Únicos: `(campaign_id, code_hmac)` y `(campaign_id, assigned_to)` cuando `assigned_to` no es nulo.

**Minimización (Ley 25.326):** los códigos no se guardan en claro. Del usuario se guarda solo su id, y solo mientras tiene el código asignado: la baja lo borra de `assigned_to`. El partner no recibe nada de eso.

## Variables

Se leen del entorno; `.env.example` documenta cada una. `.env` no se versiona.

| Variable | Valor |
|---|---|
| `APP_ENV` | `development` (por defecto) o `production` |
| `PORT` | 8080 |
| `HTTP_ADMIN_PORT` | 8082 |
| `ADMIN_API_KEY` | Mínimo 32 caracteres. Vacía: el router de administración no arranca |
| `DATABASE_URL` | Obligatoria |
| `DB_QUERY_TIMEOUT_MS` | 1500 |
| `ADMIN_DB_TIMEOUT_MS` | 30000 (una carga de 100.000 códigos tarda más que una consulta) |
| `RABBITMQ_URL` | Obligatoria |
| `RABBITMQ_PUBLISH_TIMEOUT_MS` | 2000 (publicación a la DLQ) |
| `USER_DELETED_QUEUE` | `benefits.user-deleted.q` |
| `RABBITMQ_PREFETCH` | 10 (entre 1 y 65535) |
| `MAX_RETRIES` | 5 |
| `BENEFITS_CODE_AES_KEY` | 32 bytes en base64. Obligatoria |
| `BENEFITS_CODE_HMAC_KEY` | 32 bytes en base64, independiente de la anterior. Obligatoria |
| `BANNER_MAX_BYTES` | 307200 (entre 1 y 10485760) |

La configuración se valida al arrancar y el proceso termina con código distinto de cero ante una ausencia o un valor inválido. Los errores nombran la variable, nunca su valor, y los secretos se imprimen redactados. Con `APP_ENV=production` rechaza además `sslmode=disable` en `DATABASE_URL`, las credenciales `guest` en `RABBITMQ_URL` y claves de un solo byte repetido.

Generar los secretos:

```bash
openssl rand -base64 48   # ADMIN_API_KEY
openssl rand -base64 32   # BENEFITS_CODE_AES_KEY
openssl rand -base64 32   # BENEFITS_CODE_HMAC_KEY (otro valor distinto)
```

## Cómo correrlo

```bash
docker network create vinculo-vial            # una vez; la red de la plataforma
cp .env.example .env                          # y completar los tres secretos
docker compose up --build
docker compose --profile seed run --rm vinculo-vial-benefits-seed   # datos de prueba
```

El compose levanta el servicio y su PostgreSQL (en una red interna propia, sin publicar). El RabbitMQ lo aporta la plataforma, con la topología de Core, en la red `vinculo-vial`. El contenedor corre como usuario sin privilegios sobre una imagen `scratch`.

## Datos de prueba

`cmd/seed` solo corre con `APP_ENV=development` explícito y carga, de forma idempotente:

- la campaña **"Aplicación de prueba"**, vigente, con un banner PNG generado y 100 códigos falsos (`PRUEBA-0001` a `PRUEBA-0100`);
- una campaña **vencida**, para comprobar que no se muestra.

El seed va en una etapa propia del `Dockerfile` (`target: seed`): la imagen que se publica no lo incluye.

## Pruebas

```bash
go vet ./...
export DATABASE_URL=postgres://benefits:benefits@localhost:5432/benefits?sslmode=disable
export TEST_RABBITMQ_URL=amqp://guest:guest@localhost:5672/
go test ./... -race                       # unitarias (las de PostgreSQL se omiten sin DATABASE_URL)
go test ./... -race -tags=integration     # suma las de RabbitMQ real
```

Cada prueba de PostgreSQL trabaja en su propio esquema (`search_path`), que borra al terminar, así que los paquetes corren en paralelo contra la misma base. Se prueban con la base real: el claim concurrente (60 pedidos contra 20 códigos, y 25 pedidos de una misma persona), los índices, el `FOR UPDATE SKIP LOCKED` y las cargas por lotes. `cmd/api` levanta el servicio completo (`run`) con servidores reales y recorre el contrato; con el broker real, la baja de un usuario llega por RabbitMQ hasta `BURNED`.

## CI

`.github/workflows/ci.yml` corre en todo `pull_request` hacia `dev` o `main` (y en los push a esas ramas).

- **Cobertura:** unitarias e integración, cada una con `-covermode=atomic -coverpkg=./...`, unidas con `gocovmerge` y convertidas a Cobertura XML con `gocover-cobertura`. El total se publica en `$GITHUB_STEP_SUMMARY` y los reportes quedan como artefacto.
- **Gate sobre el código nuevo:** `diff-cover coverage.xml --compare-branch=origin/<base> --fail-under=85`.
- **Gate sobre el total:** el repo nace con más de 85 %, así que el CI exige que el total no baje de 85 %.
- **Exclusiones de cobertura:** ninguna. No hay código generado, y no se excluye código escrito a mano.
- **Seguridad:** `govulncheck ./...` y Trivy (`fs`, `vuln,secret,misconfig`, `HIGH,CRITICAL`, `--ignore-unfixed`) tienen que pasar. Las excepciones de Trivy van solo en `.trivyignore`, con el motivo y la fecha de revisión (hoy no hay ninguna). Semgrep (`p/default`) es informativo y su resultado va al resumen.
- **Higiene:** `permissions: contents: read`, acciones de terceros fijadas por SHA (con la versión en un comentario) y Dependabot semanal para `gomod`, `github-actions` y `docker`.

En local:

```bash
go test ./... -race -covermode=atomic -coverpkg=./... -coverprofile=coverage-unit.out
go test ./... -race -tags=integration -covermode=atomic -coverpkg=./... -coverprofile=coverage-integration.out
go install github.com/wadey/gocovmerge@b5bfa59ec0ad
go install github.com/boumenot/gocover-cobertura@v1.5.0
gocovmerge coverage-unit.out coverage-integration.out > coverage.out
gocover-cobertura < coverage.out > coverage.xml
go tool cover -func=coverage.out | tail -1                          # cobertura total
pip install diff-cover==11.0.1
diff-cover coverage.xml --compare-branch=origin/dev --fail-under=85  # código nuevo
go install golang.org/x/vuln/cmd/govulncheck@v1.9.0 && govulncheck ./...
trivy fs --scanners vuln,secret,misconfig --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 .
semgrep scan --config p/default
```

## Release

`.github/workflows/publish.yml` se dispara con un tag `vX.Y.Z` puesto sobre `main`:

```bash
git checkout main && git pull
git tag v1.0.0
git push origin v1.0.0
```

1. El job `test` verifica que el tag apunte a un commit de `main` y corre las mismas pruebas que el CI (unitarias e integración, con PostgreSQL y RabbitMQ). Si algo falla, no se publica nada.
2. El job `publish` inicia sesión con los secrets `DOCKERHUB_USERNAME` y `DOCKERHUB_TOKEN`, y sube con Buildx `felaufernandezbottaro/vinculo-vial-benefits` con dos tags: la versión sin la `v` (`1.0.0`) y `latest`.

## Seguridad (OWASP Top 10:2025)

Categorías evaluadas y mitigaciones:

- **A01 Control de acceso:** denegación por defecto. Las rutas de `/api/v1` exigen `X-User-Id`; las de carga y de reporte van solo en el router de administración, con clave y sin ruta en Kong. Cada usuario solo ve y recibe su propio código (no hay ids ajenos en ninguna ruta). El servicio no hace llamadas salientes: `open_url` solo se valida y la abre la app.
- **A02 Configuración:** imagen `scratch`, usuario no root, sin modo de depuración ni stack traces; el puerto de administración solo en loopback; la configuración rechaza los valores inseguros en producción.
- **A03 Cadena de suministro:** `go.sum` versionado, imágenes con versión fijada, acciones por SHA, govulncheck y Trivy en el CI.
- **A04 Criptografía:** AES-256-GCM con nonce aleatorio (`crypto/rand`) y HMAC-SHA256 de la biblioteca estándar; secretos comparados a tiempo constante.
- **A05 Inyección:** consultas parametrizadas; ninguna concatenación con entrada externa.
- **A06 Diseño:** límites de tamaño en banner, JSON, multipart y CSV; el banner se valida por firma de bytes; el CSV se valida línea por línea; rate limit en Kong.
- **A07 Autenticación:** la clave de administración se compara sobre sus SHA-256 en tiempo constante; mensajes genéricos.
- **A08 Integridad:** todo JSON se decodifica con campos estrictos; el evento se valida contra su esquema antes de procesarse.
- **A09 Registro:** logs estructurados con `request_id`; se registran las denegaciones y la DLQ, nunca códigos ni la clave. El logger de GORM está apagado porque imprimiría los parámetros de las consultas lentas (por ejemplo, `code_enc`).
- **A10 Excepciones:** ante un error de la base, `503` con `Retry-After`; el `panic` de un handler va a un `500` genérico y el de un consumidor, a la DLQ.

Riesgos residuales:

- Quien alcance el puerto público sin pasar por Kong puede falsificar `X-User-Id`. El `docker-compose.yml` de desarrollo publica ese puerto, igual que el resto de los servicios; en AWS solo debe ser alcanzable desde Kong.
- La clave de administración es un secreto compartido: quien la tenga puede cargar y reportar. Se protege con el puerto aparte (solo VPC en AWS) y su largo mínimo.
- Los códigos son legibles para quien tenga `BENEFITS_CODE_AES_KEY`; rotar cualquiera de las dos claves deja inservibles los códigos ya cargados (no hay rotación en línea).
- «Uno por persona» se apoya en que cada cuenta corresponde a un DNI validado por Identity; este servicio no lo verifica.
- Un usuario que pide un código y borra su cuenta consume un código del pool (pasa a `BURNED` y no vuelve).

## Alcance y decisiones

Lo que el contrato no fija y se resolvió así:

- **Listado de administración:** `{ "items": [...] }` (como los routers de administración de US-17).
- **Banner:** se sirve mientras la campaña está vigente, aunque se hayan agotado los códigos.
- **Esquema:** `AutoMigrate`, igual que Vehicle (no hay migraciones versionadas).
- **Códigos:** se comparan exactos (distinguen mayúsculas) y se entregan en el orden en que el motor los encuentra.
