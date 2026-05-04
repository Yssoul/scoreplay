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
	"time"

	"github.com/google/uuid"

	"github.com/ygalmessas/scoreplay/internal/blobstore"
	"github.com/ygalmessas/scoreplay/internal/httpx"
)

// mediaRepository is the contract the handler depends on.
type mediaRepository interface {
	Create(ctx context.Context, m Media, tagIDs []uuid.UUID) error
	MissingTags(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error)
	Get(ctx context.Context, id uuid.UUID) (Media, []Tag, error)
	GetMetadata(ctx context.Context, id uuid.UUID) (Media, error)
}

type blobStore interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Open(ctx context.Context, key string) (io.ReadSeekCloser, error)
	Delete(ctx context.Context, key string) error
}

// Handler serves the HTTP endpoints rooted at /media.
type Handler struct {
	repo  mediaRepository
	store blobStore

	// maxUploadBytes caps the size of the multipart body.
	maxUploadBytes int64
}

// NewHandler wires the dependencies.
func NewHandler(repo mediaRepository, store blobStore, maxUploadBytes int64) *Handler {
	return &Handler{repo: repo, store: store, maxUploadBytes: maxUploadBytes}
}

// multipartMaxMemory bounds in-RAM buffering of multipart parts
// before they spill to a temp file.
const multipartMaxMemory = 10 << 20 // 10 MiB

// sniffSize is the number of leading bytes inspected by
// http.DetectContentType. The constant comes from the stdlib contract.
const sniffSize = 512

// blobCompensationTimeout caps the cleanup Delete that compensates a
// failed media insert.
const blobCompensationTimeout = 5 * time.Second

type createMediaResponse struct {
	ID      uuid.UUID   `json:"id"`
	Name    string      `json:"name"`
	Tags    []uuid.UUID `json:"tags"`
	FileURL string      `json:"fileUrl"`
}

func mediaFileURL(id uuid.UUID) string {
	return "/media/" + id.String() + "/file"
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
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
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
		FileURL: mediaFileURL(m.ID),
	}
	if resp.Tags == nil {
		resp.Tags = []uuid.UUID{}
	}
	_ = httpx.WriteJSON(w, http.StatusCreated, resp)
}

// createMedia is the transport-agnostic core of POST /media.
func (h *Handler) createMedia(ctx context.Context, in createMediaInput) (Media, error) {
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
	// file_key = media.id: a single identifier addresses both rows.
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
		// Detached, time-bounded ctx: a client disconnect must not
		// abort the cleanup and orphan the blob.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), blobCompensationTimeout)
		defer cancel()
		if delErr := h.store.Delete(cleanupCtx, fileKey); delErr != nil && !errors.Is(delErr, blobstore.ErrNotFound) {
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

// writeCreateError maps domain errors from createMedia onto HTTP
// statuses, keeping httpx out of the domain method.
func (h *Handler) writeCreateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrUnknownTags):
		httpx.WriteError(w, r, fmt.Errorf("%w: %w", httpx.ErrUnprocessable, err))
	default:
		httpx.WriteError(w, r, fmt.Errorf("%w: %w", httpx.ErrInternal, err))
	}
}

// getMediaResponse is the DTO returned by GET /media/{id}.
type getMediaResponse struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	Tags    []Tag     `json:"tags"`
	FileURL string    `json:"fileUrl"`
}

// Get handles GET /media/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rawID := r.PathValue("id")
	id, err := uuid.Parse(rawID)
	if err != nil {
		httpx.WriteError(w, r, fmt.Errorf("%w: %q is not a valid uuid: %w", httpx.ErrBadRequest, rawID, err))
		return
	}

	m, tags, err := h.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, ErrMediaNotFound) {
			httpx.WriteError(w, r, fmt.Errorf("%w: %w", httpx.ErrNotFound, err))
			return
		}
		httpx.WriteError(w, r, fmt.Errorf("%w: get media: %w", httpx.ErrInternal, err))
		return
	}

	if tags == nil {
		tags = []Tag{}
	}

	_ = httpx.WriteJSON(w, http.StatusOK, getMediaResponse{
		ID:      m.ID,
		Name:    m.Name,
		Tags:    tags,
		FileURL: mediaFileURL(m.ID),
	})
}

// ServeFile handles GET /media/{id}/file by streaming the raw blob
// to the client.
func (h *Handler) ServeFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rawID := r.PathValue("id")
	id, err := uuid.Parse(rawID)
	if err != nil {
		httpx.WriteError(w, r, fmt.Errorf("%w: %q is not a valid uuid: %w", httpx.ErrBadRequest, rawID, err))
		return
	}

	m, err := h.repo.GetMetadata(ctx, id)
	if err != nil {
		if errors.Is(err, ErrMediaNotFound) {
			httpx.WriteError(w, r, fmt.Errorf("%w: %w", httpx.ErrNotFound, err))
			return
		}
		httpx.WriteError(w, r, fmt.Errorf("%w: get metadata: %w", httpx.ErrInternal, err))
		return
	}

	rc, err := h.store.Open(ctx, m.FileKey)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			httpx.LoggerFrom(ctx).ErrorContext(ctx, "missing blob for existing media row",
				slog.String("media_id", m.ID.String()),
				slog.String("file_key", m.FileKey),
			)
			httpx.WriteError(w, r, fmt.Errorf("%w: blob missing", httpx.ErrNotFound))
			return
		}
		httpx.WriteError(w, r, fmt.Errorf("%w: open blob: %w", httpx.ErrInternal, err))
		return
	}
	defer func() { _ = rc.Close() }()

	// Set Content-Type explicitly (from DB) so http.ServeContent does
	// not try to sniff again from the bytes we are about to serve.
	w.Header().Set("Content-Type", m.ContentType)

	// The "name" passed to ServeContent is only used for sniffing if
	// Content-Type is empty — we set it above, so this argument has
	// no observable effect. Pass the file_key so any future log
	// sampler at least sees a meaningful identifier.
	http.ServeContent(w, r, m.FileKey, time.Time{}, rc)
}

// writeMultipartParseError maps ParseMultipartForm errors to 413
// (size) or 400 (everything else).
func (h *Handler) writeMultipartParseError(w http.ResponseWriter, r *http.Request, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		httpx.WriteError(w, r, fmt.Errorf("%w: upload exceeds %d bytes", httpx.ErrPayloadTooLarge, h.maxUploadBytes))
		return
	}
	httpx.WriteError(w, r, fmt.Errorf("%w: parse multipart body: %w", httpx.ErrBadRequest, err))
}

// parseTagIDs converts the raw multipart values into UUIDs. Empty or
// nil input is valid (zero-tag media allowed). Malformed values and
// duplicates are rejected with a 400.
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

// isAllowedMediaType is a permissive image/* + video/* whitelist.
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
