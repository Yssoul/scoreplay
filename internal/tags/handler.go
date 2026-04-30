package tags

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

type TagHandler struct {
	tagRepository tagRepository
}

func NewTagHandler(repo tagRepository) *TagHandler {
	return &TagHandler{tagRepository: repo}
}

// maxRequestBodyBytes caps the size of incoming JSON payloads.
const maxRequestBodyBytes = 1 << 20 // 1 MiB

func (h *TagHandler) Create(w http.ResponseWriter, r *http.Request) {
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
		httpx.WriteError(w, r, fmt.Errorf("%w: error creating tag: %w", httpx.ErrInternal, err))
		return
	}

	if err := httpx.WriteJSON(w, http.StatusCreated, tag); err != nil {
		return
	}
}

func validate(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name is required")
	}
	return nil
}
