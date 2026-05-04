# Design

A concise log of the choices made for the ScorePlay media & tags API.
Ordered by importance: stack first, architecture next, deliberate
omissions, then a few details. Anything not written down is intended
to be discussed.

---

## 1. Technical choices

### Storage: relational DB for metadata, object storage for blobs

Two distinct workloads, two distinct stores. Mixing them (e.g. blobs
as `bytea`, or metadata in object-storage tags) optimises for neither.

**Metadata → PostgreSQL.** Media and tags are small, structured,
strongly related records with hard integrity needs:

- We want uniqueness (case-insensitive tag names via `CITEXT`) and
  referential integrity (no orphan associations) enforced
  declaratively rather than re-implemented in application code. Other
  stores can express similar invariants (Mongo schema validation,
  Dynamo conditional writes, app-level checks), but a relational
  schema is the lowest-effort way to get them at this scale.
- We need atomic "create media + N associations" — a single
  transaction, no eventually-consistent reconciliation.
- The access pattern is a flat M:N relation queried in both
  directions (`media → tags` on read, `tag → media` for the future
  search). One hop, no traversal, well within Postgres's comfort
  zone. A graph DB would only earn its keep on multi-hop traversals
  (tag-of-tag, recommendations, shortest path), which we don't have.
- The brief flags future search-by-tag; Postgres handles it in-DB
  with `pg_trgm` (fuzzy/autocomplete) and `tsvector` GIN (FTS), no
  second stateful system. If volume ever outgrows that, stream into
  Elasticsearch — Postgres stays the
  system of record.
- Rejected alternatives:
  - **Document DBs (MongoDB, DynamoDB)** would work — bidirectional
    access is solvable with a multikey index or a GSI, and embedding
    tag IDs (not full objects) avoids the rename problem. The reason
    to pass is integrity, not capability: at our scale and team size
    we'd rather get FKs, unique constraints and multi-row
    transactions declaratively than re-implement them in application
    code. Their horizontal-scale payoff is also irrelevant here.
  - **Redis / KV**: no integrity, no ad-hoc queries — cache, not
    source of truth.
  - **Elasticsearch as primary**: not a system of record, no real
    transactions.
  - **Graph DB**: our relation is flat; M:N is not a traversal
    problem.

**Blobs → object storage (filesystem today, S3 tomorrow).** Media
blobs are large, opaque, write-once, read-many, and need to be
streamable with HTTP `Range`:

- A relational DB is the wrong shape for them: storing 100 MiB blobs
  as `bytea` bloats the heap, kills `pg_dump`, and forces every
  buffer through Postgres for what is fundamentally a blob transfer.
- Object storage is the standard answer — cheap, horizontally
  scalable, designed for streaming, and CDN-friendly via presigned
  URLs.
- For the take-home the implementation is a local filesystem
  (`fsstore`) so the project runs with zero external infrastructure.
  It is hidden behind a 3-method `blobstore.Store` interface, so
  moving to S3/MinIO should not be a major undertaking — a new
  package implementing the same interface, plus a factory branch.

The contract between the two: `media.file_key` (UUIDv7) is the
foreign key into the object store. The DB owns the truth about
*which* blob belongs to *which* media; the object store owns the
bytes.

### DB stack
- **`pgx/v5`** as driver — Postgres-native types, typed errors
  usable for `23505 → 409`, native target of `sqlc`.
- **`sqlc`** for query code generation. SQL stays first-class and
  reviewable; type-safe Go on top, no runtime ORM.
- **`tern`** for migrations. Same author as `pgx`, single-file
  up/down, integrates cleanly with the test pool.
- **UUIDv7** for every primary key. Time-sortable, cheaper indexes
  than v4, friendlier for keyset pagination later.

### Language & HTTP
- **Go 1.25**, standard library `net/http` router. One less dependency
  to audit; handlers stay plain `http.Handler`, the router is
  swappable.
- **RFC 7807 `application/problem+json`** for every 4xx/5xx. Uniform
  client contract; sentinel errors in `internal/httpx` map to statuses
  and the transport layer is the single place that decides how an
  error is rendered. Internal errors are logged, never echoed in the
  body.

### Testing
- **Unit** (`go test`): handlers against in-memory fakes, fast,
  deterministic, no Docker.
- **Integration** (`-tags integration`): real Postgres 16 via
  testcontainers-go, one container per top-level test, migrations
  applied via `tern`'s Go API. Used only where Postgres semantics
  matter (FKs, ordering, uniqueness, CITEXT).

---

## 2. Architectural decisions

### Two packages side by side
`internal/tags/` and `internal/media/` are split for code
organization. They share a schema and `media` reads the `tags`
table directly via its own queries. The package
split keeps each domain's HTTP handlers, repository and migrations
close together, which makes navigation and review easier; it does
not pretend to enforce a service boundary that would only be real
with a network or interface seam between them.

### Domain types decoupled from sqlc-generated types
Repositories map `db.Tag → tags.Tag` row by row. The mapping cost is
constant per query and bounded — worth it for the anti-corruption
boundary: schema can evolve without leaking into the domain or the
HTTP contract.

### Media ↔ Tags is a join table, not an array column
A `media_tags(media_id, tag_id)` table beats `UUID[]` or `JSONB` on
**integrity** (FKs enforced both ways), **evolvability** (room for
`attached_at`, ordering, audit), and principle of least surprise.
The brief explicitly mentions future search-by-tag, which removes any
remaining ambiguity.

### `ON DELETE` split: DB owns integrity, app owns business
- `media_tags.media_id … ON DELETE CASCADE`: join rows have no
  meaning without their parent media; cascade is the *definition* of
  the relation, not a business rule.
- `media_tags.tag_id … ON DELETE RESTRICT`: deleting a tag is a
  meaningful business event. The DB refuses, the app surfaces the
  error, a human decides.

The companion rule lives outside the database: when a media is
deleted, the application is still responsible for the side effects
the database cannot reach (blob removal, audit, downstream
notifications).

### No service layer between handlers and repositories
A service layer earns its place when there are multiple entry points,
genuine branching business rules, or shared orchestration across
handlers. None apply here: HTTP is the only entry point, `POST /media`
is a single happy path with explicit compensation (validate tags →
upload blob → transactional insert → compensate on failure), and no
two handlers share behaviour. Adding one
would only move code and introduce a second error-translation hop.

The `MediaHandler.Create` method is internally split in two — public
HTTP wrapper, private transport-agnostic `createMedia` — which gives
the testability of a service without the package.

### Tag name uniqueness at the schema level
`name CITEXT UNIQUE` on `tags`. Case-insensitive uniqueness is
enforced declaratively, the only layer that holds under concurrency
and across writers. Original casing is preserved for display.
App-side lowercasing was rejected: racy, lossy, easy to forget.

---

## 3. Deliberately deferred

These are conscious omissions, not oversights. Each can be added
without reshaping what is here.

- **Authentication & rate limiting.** Out of scope; the natural seam
  is a middleware layer in `httpx`.
- **Search media by tag.** Schema and indexes are designed for it
  (`media_tags(tag_id)` index + future `pg_trgm` on `tags.name`); only
  a query and a route are missing.
- **Pagination on listings.** `GET /tags` returns the full list,
  acceptable up to ~10k rows. Keyset cursor on `(name, id)` the day
  product spec lands or volume justifies it.
- **Idempotency on `POST /media`.** A retried request creates a
  duplicate today. Standard remedy is an `Idempotency-Key` header
  backed by a small dedup table; hooks naturally into `httpx`.
- **HTTP caching on `GET /media/{id}/file`.** Blobs are immutable by
  construction (key = media UUIDv7), so `ETag`, `Cache-Control:
  immutable` and `Last-Modified` are essentially free once a CDN or
  repeat clients are in front of the API.
- **Product-level input validation.** Length caps on `name`, character
  whitelists, per-format size caps, max tags per media,
  whitespace trimming on `tags.name` and `media.name` (so `" ronaldo"`
  and `"ronaldo"` cannot coexist as distinct rows despite `CITEXT`).
  The brief is silent and the global body cap already prevents
  pathological inputs; picking numbers now would be guesswork.
  Security floor (body size, required fields, structural correctness,
  FK existence, content-type sniffing) is enforced.
- **Defensive `CHECK` constraints.** Today every write goes through
  our handlers; replicating validation in the schema becomes valuable
  the moment a second writer (psql, batch, sibling service) appears.
- **Orphan-blob garbage collection.** `POST /media` already
  best-effort deletes the blob on rollback; a periodic reconciliation
  job would close the gap on crashes.
- **Tag categorisation.** Tags are flat strings today.
  In a sports domain a `kind` discriminator (player, team, competition,
  event, venue) is the obvious next axis — it enables typed search,
  per-kind validation, and UI grouping. It is omitted on purpose: the
  brief is silent on it, modelling it now would be guesswork (enum?
  free string? FK to a `tag_kinds` table?), and adding the column
  later is a one-migration, backward-compatible change (`NULL`-able
  `kind` plus a backfill). Better to let the product requirement pick
  the shape than to lock in a guess.
- **Clock injection.** Repositories call `time.Now().UTC()` directly.
  A `Clock` abstraction (interface or `func() time.Time` field) would
  unlock deterministic tests for time-sensitive logic — but there is
  none today (no expirations, no scheduled state, no time-windowed
  queries), so the indirection would carry weight without paying rent.
  The day the first such feature lands (presigned-URL TTL, soft-delete
  retention, rate windows), repositories take a clock at construction.
- **Observability.** Pre-instrumented packages exist
  for HTTP, repository, and pgx. Logging latency and error rate is
  the immediate next step.
- **OpenAPI spec.** Not written yet: the handler surface is small,
  hand-written, and stable, so the producer-side payoff (codegen,
  drift protection) is marginal at this size. The real value is on
  the consumer side — machine-readable contract, SDK generation,
  Swagger UI, Postman import — and lands the day a second client
  appears.

---

## 4. Details worth a line

- **Listings always return `[]`, never `null`.** Pinned by both the
  repository (`make([]T, 0, len(rows))`) and the handler, asserted by
  a dedicated test. A nil slice marshals to `null` in Go and forces
  every client to add a guard.
- **`WriteJSON` errors are intentionally discarded.** Once headers and
  a status are written, there is no honest recovery path: a partial
  body cannot be unsent and switching to a Problem response would
  corrupt the wire. Client disconnects are already surfaced upstream
  as `499` by `httpx.WriteError`; the residual failure mode is an
  encode error on a type we own and test. Logging would add noise
  without enabling any action. The single `_ =` site is the seam to
  revisit if that ever changes.
- **Body size capped per endpoint** via `http.MaxBytesReader`. JSON
  endpoints cap at 1 MiB; `POST /media` uses
  `MEDIA_MAX_UPLOAD_BYTES` (default 100 MiB).
- **Connection-level `ReadTimeout`/`WriteTimeout` are zero by default.**
  They are global per-connection deadlines, which conflict with the
  100 MiB upload cap and the streaming `GET /media/{id}/file`: a slow
  but legitimate client would be reset mid-transfer at any value low
  enough to defend against attack, and any value high enough to cover
  realistic mobile uploads is no defense at all. Defenses are layered
  elsewhere: `ReadHeaderTimeout=5s` blocks slow-loris on the request
  line, `IdleTimeout=60s` evicts idle keep-alive connections,
  `http.MaxBytesReader` bounds body size per route, and request
  cancellation propagates via `context` to the DB and blob store.
  Operators who need wall-clock budgets per route can wrap the JSON
  mux with `http.TimeoutHandler`; the seam is in `newHTTPServer`.
- **Content-type sniffed on upload** (first 512 bytes), stored in
  `media.content_type`, served back without re-sniffing. Anything
  that is not `image/*` or `video/*` is rejected with 415.
- **`fsstore` key validation** as defense in depth: even though
  callers always pass UUIDv7, keys are matched against
  `^[a-zA-Z0-9_-]{1,128}$` before touching the filesystem (rules out
  `../`, separators, absurd lengths).
- **`POST /media` orchestration**: validate tags → write blob →
  transactional insert of media + `media_tags` → on DB failure, the
  blob is best-effort deleted to compensate. The compensation runs on
  a context detached from the request (`context.WithoutCancel` plus a
  short timeout) so a client disconnect mid-rollback cannot abort the
  cleanup and turn a recoverable failure into an orphan blob. The
  current `fsstore` ignores `ctx`, but the contract assumes any
  backend (S3 next) will honor it — pinned by a dedicated test. Not
  idempotent; see §3.
- **Configuration is fail-fast**: missing required env vars are
  reported all at once at startup, then the process exits.
- **`.env` is loaded by the Makefile only**; the binary itself does
  not parse dotfiles, keeping production deployments free of
  dotfile magic.

---

## 5. What I would do next

- **S3-compatible backend** (MinIO in dev, S3 in prod) + presigned
  `GET` URLs so a CDN can serve media directly.
- **Pagination on listings** before the API contract has clients
  that depend on the unbounded shape.
- **Search media by tag** — query + route on top of the existing
  schema.
- **Auth and rate limiting** as `httpx` middleware.
- **Idempotency on `POST /media`.**
- **OpenAPI spec.**
- **OpenTelemetry traces** HTTP → repository → pgx, plus the four
  golden signals (latency, traffic, errors, saturation) as
  metrics — per-route histograms on the `httpx` middleware and
  `pgxpool`
- **Orphan-blob GC** to catch what crash-time compensation misses.
