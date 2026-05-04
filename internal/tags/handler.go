package tags

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/ygalmessas/scoreplay/internal/httpx"
)

type createTagRequest struct {
	Name string `json:"name"`
}

type tagRepository interface {
	CreateTag(ctx context.Context, tag Tag) error
	ListTags(ctx context.Context) ([]Tag, error)
}

// Handler serves the HTTP endpoints rooted at /tags. The exported
// type is intentionally Handler (not TagHandler): callers reference
// it as tags.Handler, so the package qualifier already carries the
// "tag" context.
type Handler struct {
	tagRepository tagRepository
}

// NewHandler wires the repository into a Handler.
func NewHandler(repo tagRepository) *Handler {
	return &Handler{tagRepository: repo}
}

// maxRequestBodyBytes caps the size of incoming JSON payloads.
const maxRequestBodyBytes = 1 << 20 // 1 MiB

// Create handles POST /tags.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req createTagRequest
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, r, fmt.Errorf("%w: invalid json body: %w", httpx.ErrBadRequest, err))
		return
	}

	if err := validate(req.Name); err != nil {
		httpx.WriteError(w, r, fmt.Errorf("%w: invalid request body: %w", httpx.ErrBadRequest, err))
		return
	}

	id, err := uuid.NewV7()
	if err != nil {
		httpx.WriteError(w, r, fmt.Errorf("%w: error while generating tag id: %w", httpx.ErrInternal, err))
		return
	}

	tag := Tag{ID: id, Name: req.Name}
	if err := h.tagRepository.CreateTag(r.Context(), tag); err != nil {
		if errors.Is(err, ErrNameConflict) {
			httpx.WriteError(w, r, fmt.Errorf("%w: %w", httpx.ErrConflict, err))
			return
		}
		httpx.WriteError(w, r, fmt.Errorf("%w: error creating tag: %w", httpx.ErrInternal, err))
		return
	}

	httpx.LoggerFrom(r.Context()).InfoContext(r.Context(), "tag created",
		slog.String("tag_id", tag.ID.String()),
		slog.String("tag_name", tag.Name),
	)

	_ = httpx.WriteJSON(w, http.StatusCreated, tag)
}

// List returns all tags ordered by name ascending.
// The response is always a JSON array (possibly empty), never null.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	res, err := h.tagRepository.ListTags(r.Context())
	if err != nil {
		httpx.WriteError(w, r, fmt.Errorf("%w: error listing tags: %w", httpx.ErrInternal, err))
		return
	}
	if res == nil {
		res = []Tag{}
	}

	//TODO: Review if we want to log the error if WriteJSON fails.
	_ = httpx.WriteJSON(w, http.StatusOK, res)
}

func validate(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name is required")
	}
	return nil
}
