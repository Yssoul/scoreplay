package tags

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ygalmessas/scoreplay/internal/httpx"
)

type fakeTagRepository struct {
	createErr  error
	createdTag Tag
	calls      int
}

// Compile-time proof that the fake satisfies the interface the handler
// depends on. If tagRepository ever grows a method, this line fails to
// compile and points us at the tests that need updating.
var _ tagRepository = (*fakeTagRepository)(nil)

func (f *fakeTagRepository) CreateTag(_ context.Context, tag Tag) error {
	f.calls++
	f.createdTag = tag
	return f.createErr
}

// newCreateRequest builds a POST /tags request with a JSON body.
// It centralises header setup so individual tests can stay focused on
// status codes and response bodies.
func newCreateRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/tags", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestCreateTag_Created(t *testing.T) {
	repo := &fakeTagRepository{}
	h := NewTagHandler(repo)

	rec := httptest.NewRecorder()
	h.Create(rec, newCreateRequest(t, `{"name":"Messi"}`))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status: got %d, want %d (body=%q)", res.StatusCode, http.StatusCreated, rec.Body.String())
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type: got %q, want application/json*", ct)
	}

	var got Tag
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Name != "Messi" {
		t.Errorf("response name: got %q, want %q", got.Name, "Messi")
	}
	if got.ID == uuid.Nil {
		t.Error("response id: got uuid.Nil, want a generated UUID")
	}

	if repo.calls != 1 {
		t.Fatalf("repo calls: got %d, want 1", repo.calls)
	}
	if repo.createdTag.Name != "Messi" {
		t.Errorf("repo received name: got %q, want %q", repo.createdTag.Name, "Messi")
	}
	if repo.createdTag.ID != got.ID {
		t.Errorf("repo received id %s, response returned id %s (should match)", repo.createdTag.ID, got.ID)
	}
}

func TestCreateTag_BadRequest(t *testing.T) {
	// Body larger than the 1 MiB cap the handler enforces via http.MaxBytesReader.
	oversizedBody := `{"name":"` + strings.Repeat("a", maxRequestBodyBytes+1) + `"}`

	cases := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ``},
		{name: "empty name", body: `{"name":""}`},
		{name: "whitespace-only name", body: `{"name":"   "}`},
		{name: "unknown field", body: `{"name":"Messi","extra":"x"}`},
		{name: "malformed json", body: `{"name":`},
		{name: "body over size cap", body: oversizedBody},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeTagRepository{}
			h := NewTagHandler(repo)

			rec := httptest.NewRecorder()
			h.Create(rec, newCreateRequest(t, tc.body))

			res := rec.Result()
			defer res.Body.Close()

			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status: got %d, want %d (body=%q)", res.StatusCode, http.StatusBadRequest, rec.Body.String())
			}
			assertProblemJSON(t, res, http.StatusBadRequest)

			if repo.calls != 0 {
				t.Errorf("repo calls: got %d, want 0 (handler must not reach repo on validation failure)", repo.calls)
			}
		})
	}
}

func TestCreateTag_Conflict(t *testing.T) {
	repo := &fakeTagRepository{createErr: ErrNameConflict}
	h := NewTagHandler(repo)

	rec := httptest.NewRecorder()
	h.Create(rec, newCreateRequest(t, `{"name":"Messi"}`))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status: got %d, want %d (body=%q)", res.StatusCode, http.StatusConflict, rec.Body.String())
	}
	assertProblemJSON(t, res, http.StatusConflict)

	if repo.calls != 1 {
		t.Errorf("repo calls: got %d, want 1", repo.calls)
	}
}

func TestCreateTag_InternalError(t *testing.T) {
	// A generic, non-sentinel error: the handler must treat anything that
	// is not ErrNameConflict as a 500 and must not leak the underlying
	// message to the client.
	boom := errors.New("unexpected boom: db unreachable")
	repo := &fakeTagRepository{createErr: boom}
	h := NewTagHandler(repo)

	rec := httptest.NewRecorder()
	h.Create(rec, newCreateRequest(t, `{"name":"Messi"}`))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want %d (body=%q)", res.StatusCode, http.StatusInternalServerError, rec.Body.String())
	}

	var problem httpx.Problem
	if err := json.NewDecoder(res.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Status != http.StatusInternalServerError {
		t.Errorf("problem.status: got %d, want %d", problem.Status, http.StatusInternalServerError)
	}
	if strings.Contains(problem.Detail, boom.Error()) {
		t.Errorf("problem.detail leaked internal error: %q contains %q", problem.Detail, boom.Error())
	}
	if repo.calls != 1 {
		t.Errorf("repo calls: got %d, want 1", repo.calls)
	}
}

// assertProblemJSON verifies the response carries an RFC 7807
// application/problem+json body whose status field matches the HTTP status.
// Kept in one place so every 4xx/5xx test exercises the same contract.
func assertProblemJSON(t *testing.T, res *http.Response, wantStatus int) {
	t.Helper()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("content-type: got %q, want application/problem+json*", ct)
	}
	var problem httpx.Problem
	if err := json.NewDecoder(res.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Status != wantStatus {
		t.Errorf("problem.status: got %d, want %d", problem.Status, wantStatus)
	}
	if problem.Title == "" {
		t.Error("problem.title: empty, expected a non-empty title")
	}
}
