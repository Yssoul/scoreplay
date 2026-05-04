# ScorePlay — Media & Tags API

A small HTTP service that lets ScorePlay store, tag and serve sports media
(photos and videos). Written as a take-home exercise; the original brief is
preserved verbatim in [`ASSIGNMENT.md`](./ASSIGNMENT.md).

The architecture and the rationale behind every non-trivial decision are
documented in [`DESIGN.md`](./DESIGN.md). This README is intentionally
short: it tells you how to run the service, hit the API, and find your way
around the code.

---

## TL;DR

```bash
make dev          # docker compose up + migrate + go run ./cmd/api
make test         # fast unit tests, no Docker
make test-integration   # adds Postgres-backed integration tests (Docker required)
make lint         # golangci-lint v2
```

The API listens on `:8080` by default. There are six endpoints:

| Method | Path                  | Purpose                              |
|--------|-----------------------|--------------------------------------|
| POST   | `/tags`               | Create a tag                         |
| GET    | `/tags`               | List all tags                        |
| POST   | `/media`              | Upload a media file with tags        |
| GET    | `/media/{id}`         | Fetch media metadata                 |
| GET    | `/media/{id}/file`    | Stream the raw file (Range-aware)    |
| GET    | `/healthz`            | Liveness probe                       |

> **Out of scope for this take-home:** searching media by tag (`GET /media?tag=...`)
> and authentication. See `DESIGN.md` for the full list of deliberate omissions.

---

## Requirements

- **Go 1.25**
- **Docker** (for Postgres and integration tests)
- **`tern`** for migrations: `go install github.com/jackc/tern/v2@latest`
- **`golangci-lint` v2** (optional, only for `make lint`)

The service depends on a single external system: a Postgres 16 instance
provided by `docker-compose.yaml`. Blobs are written to the local
filesystem under `./var/blobs` (configurable via `MEDIA_BLOB_DIR`).

---

## Configuration

All configuration is read from environment variables at startup. Copy
`.env.example` to `.env` and adjust as needed; the `Makefile` loads `.env`
into every recipe automatically.

| Variable                  | Default            | Description                              |
|---------------------------|--------------------|------------------------------------------|
| `PGHOST` / `PGPORT` / …   | see `.env.example` | Postgres connection. Combined into a DSN.|
| `HTTP_ADDR`               | `:8080`            | Listen address.                          |
| `HTTP_READ_TIMEOUT`       | `15s`              | Server read timeout.                     |
| `HTTP_WRITE_TIMEOUT`      | `15s`              | Server write timeout.                    |
| `HTTP_SHUTDOWN_TIMEOUT`   | `10s`              | Graceful shutdown grace period.          |
| `MEDIA_MAX_UPLOAD_BYTES`  | `100 MiB`          | Hard cap on `POST /media` body size.     |
| `MEDIA_BLOB_DIR`          | `./var/blobs`      | Local blob store root.                   |

If a required variable is missing, the service prints **all** configuration
errors at once and exits with a non-zero status (fail-fast at boot).

---

## Running locally

```bash
# 1. Bring up Postgres and apply migrations.
make db-up
make migrate

# 2. Run the API.
make run
```

Or in one shot:

```bash
make dev
```

Tear down:

```bash
make db-down
```

---

## API examples

All error responses follow [RFC 7807](https://datatracker.ietf.org/doc/html/rfc7807)
(`application/problem+json`). Successful responses are plain JSON, with the
exception of `GET /media/{id}/file` which streams the raw bytes.

### Create a tag

```bash
curl -s -X POST http://localhost:8080/tags \
  -H 'Content-Type: application/json' \
  -d '{"name":"messi"}'
```

```json
{"id":"01926d4f-...","name":"messi"}
```

A duplicate name (case-insensitive) returns `409 Conflict`.

### List tags

```bash
curl -s http://localhost:8080/tags
```

```json
[{"id":"01926d4f-...","name":"messi"}]
```

The response is **always** a JSON array — empty (`[]`), never `null`.

### Upload a media

```bash
curl -s -X POST http://localhost:8080/media \
  -F 'name=Goal of the season' \
  -F 'tags=01926d4f-...' \
  -F 'tags=01926d50-...' \
  -F 'file=@./goal.jpg'
```

```json
{
  "id":"01926d51-...",
  "name":"Goal of the season",
  "tags":["01926d4f-...","01926d50-..."],
  "file_url":"/media/01926d51-.../file"
}
```

The `tags` field is a list of **tag IDs** that must already exist (any
unknown id yields `400 Bad Request` and the upload is aborted before
anything is persisted). The `file` field carries the binary; the server
sniffs its content type from the first 512 bytes and rejects anything that
is not an image or a video with `415 Unsupported Media Type`.

### Fetch media metadata

```bash
curl -s http://localhost:8080/media/01926d51-...
```

```json
{
  "id":"01926d51-...",
  "name":"Goal of the season",
  "tags":[
    {"id":"01926d4f-...","name":"messi"},
    {"id":"01926d50-...","name":"barcelona"}
  ],
  "file_url":"/media/01926d51-.../file"
}
```

### Stream the file

```bash
curl -sO -J http://localhost:8080/media/01926d51-.../file
```

The endpoint sets `Content-Type` from the value detected at upload time
and honors HTTP `Range` requests, so it works as a video source for
`<video>` tags out of the box.

---

## Project layout

```
cmd/api/                  # entrypoint, wiring, graceful shutdown
internal/
  config/                 # env-driven Config + validation
  httpx/                  # shared HTTP helpers (RFC 7807, logger context)
  postgres/               # pgxpool bootstrap
  blobstore/              # Store interface + fsstore implementation
  tags/                   # POST/GET /tags: handler, repository, model
  media/                  # POST/GET /media: handler, repository, model
  */integration/          # Postgres-backed tests (build tag: integration)
  */db/                   # sqlc-generated code (do not edit by hand)
db/
  migrations/             # tern migrations
  queries/                # sqlc input
DESIGN.md                 # architecture + every non-obvious decision
ASSIGNMENT.md             # original take-home brief
```

Two bounded contexts (`tags` and `media`) live side by side under
`internal/`, each with its own handler, repository and migrations. They
communicate only through stable IDs — `media` does not import `tags` —
which keeps the seams visible and the test surface small.

---

## Testing

Two layers, with a clear separation of concerns:

- **Unit tests** (default `go test`) — exercise handlers against in-memory
  fakes for the repository and the blob store. Fast, deterministic, no
  external dependency.
- **Integration tests** (`-tags integration`) — spin a real Postgres via
  [testcontainers-go](https://golang.testcontainers.org/) and verify the
  repositories and the database contracts (foreign keys, ordering,
  uniqueness). Used only where Postgres semantics matter; `DESIGN.md §6`
  explains why we resist the temptation to mirror every unit test here.

```bash
make test                # unit only
make test-integration    # unit + integration (Docker required)
```

---

## Where to read next

- [`DESIGN.md`](./DESIGN.md) — language & runtime, HTTP layer, storage
  layer, blob store, transactional integrity, testing strategy, and the
  list of explicit non-goals (auth, search, idempotency on `POST /media`,
  etc.).
- [`ASSIGNMENT.md`](./ASSIGNMENT.md) — the original take-home brief, kept
  verbatim for traceability.
