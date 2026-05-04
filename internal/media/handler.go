package media

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/ygalmessas/scoreplay/internal/blobstore"
	"github.com/ygalmessas/scoreplay/internal/httpx"
)

// mediaRepository is the contract the handler depends on.
type mediaRepository interface {
	Create(ctx context.Context, m Media, tagIDs []uuid.UUID) error
	MissingTags(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error)
}

type blobStore interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Delete(ctx context.Context, key string) error
}

// MediaHandler serves the HTTP endpoints rooted at /media.
type MediaHandler struct {
	repo  mediaRepository
	store blobStore

	// maxUploadBytes caps the size of the multipart body. Any byte
	// past the limit is rejected with 413 by http.MaxBytesReader.
	maxUploadBytes int64
}

// NewMediaHandler wires the dependencies.
func NewMediaHandler(repo mediaRepository, store blobStore, maxUploadBytes int64) *MediaHandler {
	return &MediaHandler{repo: repo, store: store, maxUploadBytes: maxUploadBytes}
}

// multipartMaxMemory is how much of the multipart body Go is allowed
// to keep in RAM before spilling to a temp file on disk. Generous
// enough for the small text fields, small enough that a burst of
// concurrent uploads cannot exhaust memory.
const multipartMaxMemory = 10 << 20 // 10 MiB

// sniffSize is the number of leading bytes inspected by
// http.DetectContentType. The constant comes from the stdlib contract.
const sniffSize = 512

type createMediaResponse struct {
	ID      uuid.UUID   `json:"id"`
	Name    string      `json:"name"`
	Tags    []uuid.UUID `json:"tags"`
	FileURL string      `json:"fileUrl"`
}

// createMediaInput carries the fully-parsed, fully-validated inputs
// the domain flow needs. Lifting this struct out of *http.Request is
// what lets createMedia stay transport-agnostic.
type createMediaInput struct {
	Name        string
	TagIDs      []uuid.UUID
	ContentType string
	Body        io.Reader
}

// Create handles POST /media.
func (h *MediaHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadBytes)

	if err := r.ParseMultipartForm(multipartMaxMemory); err != nil {
		h.writeMultipartParseError(w, r, err)
		return
	}
	// ParseMultipartForm allocates temp files for parts that exceed
	// multipartMaxMemory; cleanup is best-effort and does not affect
	// the response.
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		httpx.WriteError(w, r, fmt.Errorf("%w: name is required", httpx.ErrBadRequest))
		return
	}

	tagIDs, err := parseTagIDs(r.PostForm["tags"])
	if err != nil {
		httpx.WriteError(w, r, fmt.Errorf("%w: %w", httpx.ErrBadRequest, err))
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.WriteError(w, r, fmt.Errorf("%w: file is required: %w", httpx.ErrBadRequest, err))
		return
	}
	defer func() { _ = file.Close() }()

	if header.Size == 0 {
		httpx.WriteError(w, r, fmt.Errorf("%w: file is empty", httpx.ErrBadRequest))
		return
	}

	// bufio.Peek lets us sniff the leading bytes without consuming
	// them: the same reader is then fed in full to the blob store.
	br := bufio.NewReaderSize(file, sniffSize)
	head, err := br.Peek(sniffSize)
	// io.EOF / io.ErrUnexpectedEOF mean the file is shorter than 512
	// bytes; we still get the bytes available and can sniff them.
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		httpx.WriteError(w, r, fmt.Errorf("%w: read upload: %w", httpx.ErrInternal, err))
		return
	}
	contentType := http.DetectContentType(head)
	if !isAllowedMediaType(contentType) {
		httpx.WriteError(w, r, fmt.Errorf("%w: %s is not an image or video", httpx.ErrUnsupportedMediaType, contentType))
		return
	}

	m, err := h.createMedia(ctx, createMediaInput{
		Name:        name,
		TagIDs:      tagIDs,
		ContentType: contentType,
		Body:        br,
	})
	if err != nil {
		h.writeCreateError(w, r, err)
		return
	}

	httpx.LoggerFrom(ctx).InfoContext(ctx, "media created",
		slog.String("media_id", m.ID.String()),
		slog.String("content_type", m.ContentType),
		slog.Int("tag_count", len(tagIDs)),
	)

	resp := createMediaResponse{
		ID:      m.ID,
		Name:    m.Name,
		Tags:    tagIDs,
		FileURL: "/media/" + m.ID.String() + "/file",
	}
	if resp.Tags == nil {
		resp.Tags = []uuid.UUID{}
	}
	_ = httpx.WriteJSON(w, http.StatusCreated, resp)
}

// createMedia is the domain orchestration of POST /media: validate
// foreign keys, generate the id, stream the blob, persist the row,
// compensate on failure. It is transport-agnostic on purpose so the
// HTTP wrapper above stays focused on parsing and status mapping.
func (h *MediaHandler) createMedia(ctx context.Context, in createMediaInput) (Media, error) {
	// Pre-validate the tag set before writing the blob. Unknown tags
	// are an upstream client mistake, not an internal failure, and
	// catching them here avoids an orphan blob on a doomed
	// transaction.
	if len(in.TagIDs) > 0 {
		missing, err := h.repo.MissingTags(ctx, in.TagIDs)
		if err != nil {
			return Media{}, fmt.Errorf("validate tags: %w", err)
		}
		if len(missing) > 0 {
			return Media{}, fmt.Errorf("%w: %s", ErrUnknownTags, joinUUIDs(missing))
		}
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Media{}, fmt.Errorf("generate media id: %w", err)
	}
	// file_key = media.id keeps a single source of truth for
	// addressing the asset and makes "find this media's blob"
	// trivial in ops.
	fileKey := id.String()

	if err := h.store.Put(ctx, fileKey, in.Body); err != nil {
		return Media{}, fmt.Errorf("store blob: %w", err)
	}

	m := Media{
		ID:          id,
		Name:        in.Name,
		FileKey:     fileKey,
		ContentType: in.ContentType,
	}
	if err := h.repo.Create(ctx, m, in.TagIDs); err != nil {
		// Compensation: best-effort cleanup of the blob we just
		// wrote. We log but do not surface the cleanup failure: the
		// user-facing error is the original DB failure. A periodic
		// reconciliation job (DESIGN.md §7) catches what slips
		// through here.
		if delErr := h.store.Delete(ctx, fileKey); delErr != nil && !errors.Is(delErr, blobstore.ErrNotFound) {
			httpx.LoggerFrom(ctx).ErrorContext(ctx, "orphan blob: failed to compensate after db error",
				slog.String("file_key", fileKey),
				slog.Any("delete_err", delErr),
				slog.Any("db_err", err),
			)
		}
		return Media{}, fmt.Errorf("persist media: %w", err)
	}

	return m, nil
}

// writeCreateError translates the domain-level errors returned by
// createMedia into RFC 7807 responses. Keeping the mapping in one
// place stops the domain method from importing httpx and makes the
// HTTP contract trivial to audit.
func (h *MediaHandler) writeCreateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrUnknownTags):
		httpx.WriteError(w, r, fmt.Errorf("%w: %w", httpx.ErrUnprocessable, err))
	default:
		httpx.WriteError(w, r, fmt.Errorf("%w: %w", httpx.ErrInternal, err))
	}
}

// writeMultipartParseError maps the various failure modes of
// ParseMultipartForm to the right HTTP status. http.MaxBytesError is
// 413; everything else (truncated stream, missing boundary…) is a
// client mistake and surfaces as 400.
func (h *MediaHandler) writeMultipartParseError(w http.ResponseWriter, r *http.Request, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		httpx.WriteError(w, r, fmt.Errorf("%w: upload exceeds %d bytes", httpx.ErrPayloadTooLarge, h.maxUploadBytes))
		return
	}
	httpx.WriteError(w, r, fmt.Errorf("%w: parse multipart body: %w", httpx.ErrBadRequest, err))
}

// parseTagIDs converts the raw multipart values into UUIDs. Empty or
// nil input is valid: the brief allows a media with zero tags. A
// single malformed value rejects the whole request (fail-fast).
// Duplicates are rejected up-front: letting them through would either
// violate the (media_id, tag_id) primary key on insert (surfacing as
// a 500) or rely on silent dedup downstream — both worse than a
// crisp 400.
func parseTagIDs(raw []string) ([]uuid.UUID, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]uuid.UUID, 0, len(raw))
	seen := make(map[uuid.UUID]struct{}, len(raw))
	for _, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, errors.New("tags contains an empty value")
		}
		id, err := uuid.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("tag %q is not a valid uuid: %w", v, err)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("tag %s is duplicated", id)
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// isAllowedMediaType keeps the policy in one place. A permissive
// image/* + video/* whitelist filters the obviously wrong (PDF,
// executables, application/octet-stream) without rejecting legitimate
// formats we have not anticipated (HEIC, AVIF, …).
func isAllowedMediaType(ct string) bool {
	// Strip parameters (e.g. "; charset=utf-8") that DetectContentType
	// can append on text formats. We only care about the type/subtype.
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	return strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "video/")
}

// joinUUIDs renders a slice of UUIDs as a comma-separated string for
// inclusion in a 4xx error body. Stable order.
func joinUUIDs(ids []uuid.UUID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = id.String()
	}
	return strings.Join(s, ", ")
}
