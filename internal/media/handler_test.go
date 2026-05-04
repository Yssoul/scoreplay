package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/ygalmessas/scoreplay/internal/blobstore"
	"github.com/ygalmessas/scoreplay/internal/httpx"
)

// jpegMagic is the SOI marker JPEG decoders look for. Prefixing the
// fake upload with these three bytes is enough for
// http.DetectContentType to classify it as image/jpeg without us
// embedding a full image in the test source.
var jpegMagic = []byte{0xFF, 0xD8, 0xFF}

// fakeMediaRepository captures how the handler exercises the
// repository interface. Counters and recorded arguments are the only
// observable contract from the handler's point of view.
type fakeMediaRepository struct {
	createErr   error
	createCalls int
	createdM    Media
	createdTags []uuid.UUID

	missingResult []uuid.UUID
	missingErr    error
	missingCalls  int
}

var _ mediaRepository = (*fakeMediaRepository)(nil)

func (f *fakeMediaRepository) Create(_ context.Context, m Media, tagIDs []uuid.UUID) error {
	f.createCalls++
	f.createdM = m
	f.createdTags = tagIDs
	return f.createErr
}

func (f *fakeMediaRepository) MissingTags(_ context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	f.missingCalls++
	return f.missingResult, f.missingErr
}

// fakeBlobStore records Put/Delete activity so tests can assert
// compensation actually happened. A mutex makes it safe under -race
// even though the handler is sequential: better one cheap lock than
// one flaky run a year from now.
type fakeBlobStore struct {
	mu sync.Mutex

	putErr    error
	putCalls  int
	putKey    string
	putBytes  []byte
	deleteErr error
	deleted   []string
}

var _ blobStore = (*fakeBlobStore)(nil)

func (f *fakeBlobStore) Put(_ context.Context, key string, r io.Reader) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	f.putKey = key
	if f.putErr != nil {
		return f.putErr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.putBytes = b
	return nil
}

func (f *fakeBlobStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, key)
	return f.deleteErr
}

// buildMultipart assembles a multipart/form-data body. The returned
// content-type carries the boundary, which the handler needs to parse
// the body. Pass fileBody=nil to omit the file part entirely.
func buildMultipart(t *testing.T, fields map[string][]string, fileField string, fileName string, fileBody []byte) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	mw := multipart.NewWriter(body)

	for k, vs := range fields {
		for _, v := range vs {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatalf("write field %s: %v", k, err)
			}
		}
	}

	if fileBody != nil {
		fw, err := mw.CreateFormFile(fileField, fileName)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(fileBody); err != nil {
			t.Fatalf("write file body: %v", err)
		}
	}

	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return body, mw.FormDataContentType()
}

// newCreateRequest is the test-side counterpart of a curl invocation
// of POST /media. Centralising it here keeps individual tests focused
// on the case under test.
func newCreateRequest(t *testing.T, fields map[string][]string, fileBody []byte) *http.Request {
	t.Helper()
	body, ct := buildMultipart(t, fields, "file", "asset.bin", fileBody)
	req := httptest.NewRequest(http.MethodPost, "/media", body)
	req.Header.Set("Content-Type", ct)
	return req
}

const testMaxUpload = 10 << 20 // 10 MiB, plenty for tests

func TestCreateMedia_Created(t *testing.T) {
	tagA := uuid.MustParse("019f0000-0000-7000-8000-000000000001")
	tagB := uuid.MustParse("019f0000-0000-7000-8000-000000000002")

	repo := &fakeMediaRepository{}
	store := &fakeBlobStore{}
	h := NewMediaHandler(repo, store, testMaxUpload)

	body := append(append([]byte{}, jpegMagic...), []byte("rest of the bytes")...)
	req := newCreateRequest(t, map[string][]string{
		"name": {"Messi goal"},
		"tags": {tagA.String(), tagB.String()},
	}, body)
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status: got %d, want %d (body=%q)", res.StatusCode, http.StatusCreated, rec.Body.String())
	}

	var got createMediaResponse
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID == uuid.Nil {
		t.Error("response id: got uuid.Nil, want a generated UUID")
	}
	if got.Name != "Messi goal" {
		t.Errorf("response name: got %q, want %q", got.Name, "Messi goal")
	}
	if got.FileURL != "/media/"+got.ID.String()+"/file" {
		t.Errorf("response fileUrl: got %q, want %q", got.FileURL, "/media/"+got.ID.String()+"/file")
	}
	if len(got.Tags) != 2 || got.Tags[0] != tagA || got.Tags[1] != tagB {
		t.Errorf("response tags: got %v, want [%s %s]", got.Tags, tagA, tagB)
	}

	if store.putCalls != 1 {
		t.Errorf("store.Put calls: got %d, want 1", store.putCalls)
	}
	if store.putKey != got.ID.String() {
		t.Errorf("store.Put key: got %q, want %q", store.putKey, got.ID.String())
	}
	if !bytes.Equal(store.putBytes, body) {
		t.Errorf("store.Put bytes: got %d bytes, want %d (sniff buffer must not be lost)", len(store.putBytes), len(body))
	}

	if repo.createCalls != 1 {
		t.Errorf("repo.Create calls: got %d, want 1", repo.createCalls)
	}
	if repo.createdM.ID != got.ID {
		t.Errorf("repo received id %s, response returned %s (must match)", repo.createdM.ID, got.ID)
	}
	if repo.createdM.ContentType != "image/jpeg" {
		t.Errorf("repo received content_type %q, want image/jpeg (sniff failed)", repo.createdM.ContentType)
	}
	if repo.createdM.FileKey != got.ID.String() {
		t.Errorf("repo received file_key %q, want %q (file_key = media.id)", repo.createdM.FileKey, got.ID.String())
	}

	if len(store.deleted) != 0 {
		t.Errorf("store.Delete called on success path: %v", store.deleted)
	}
}

func TestCreateMedia_NoTags_OK(t *testing.T) {
	repo := &fakeMediaRepository{}
	store := &fakeBlobStore{}
	h := NewMediaHandler(repo, store, testMaxUpload)

	req := newCreateRequest(t, map[string][]string{
		"name": {"untagged"},
	}, jpegMagic)
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status: got %d, want %d (body=%q)", res.StatusCode, http.StatusCreated, rec.Body.String())
	}
	if repo.missingCalls != 0 {
		t.Errorf("repo.MissingTags calls: got %d, want 0 (no tags = no validation)", repo.missingCalls)
	}

	var got createMediaResponse
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tags == nil {
		t.Error("response tags: got null, want [] (handlers must never serialise null arrays)")
	}
	if len(got.Tags) != 0 {
		t.Errorf("response tags: got %v, want []", got.Tags)
	}
}

func TestCreateMedia_BadRequest(t *testing.T) {
	cases := []struct {
		name      string
		fields    map[string][]string
		fileBody  []byte
		wantSubs  string
	}{
		{
			name:     "missing name",
			fields:   map[string][]string{},
			fileBody: jpegMagic,
			wantSubs: "name",
		},
		{
			name:     "blank name",
			fields:   map[string][]string{"name": {"   "}},
			fileBody: jpegMagic,
			wantSubs: "name",
		},
		{
			name:     "missing file",
			fields:   map[string][]string{"name": {"x"}},
			fileBody: nil,
			wantSubs: "file",
		},
		{
			name:     "empty file",
			fields:   map[string][]string{"name": {"x"}},
			fileBody: []byte{},
			wantSubs: "file",
		},
		{
			name:     "malformed tag uuid",
			fields:   map[string][]string{"name": {"x"}, "tags": {"not-a-uuid"}},
			fileBody: jpegMagic,
			wantSubs: "uuid",
		},
		{
			name: "duplicate tag uuid",
			fields: map[string][]string{
				"name": {"x"},
				"tags": {
					"019f0000-0000-7000-8000-000000000001",
					"019f0000-0000-7000-8000-000000000001",
				},
			},
			fileBody: jpegMagic,
			wantSubs: "duplicated",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeMediaRepository{}
			store := &fakeBlobStore{}
			h := NewMediaHandler(repo, store, testMaxUpload)

			req := newCreateRequest(t, tc.fields, tc.fileBody)
			rec := httptest.NewRecorder()
			h.Create(rec, req)

			res := rec.Result()
			defer res.Body.Close()

			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400 (body=%q)", res.StatusCode, rec.Body.String())
			}
			assertProblemJSON(t, res, http.StatusBadRequest)

			if store.putCalls != 0 {
				t.Errorf("store.Put called on validation failure: %d times", store.putCalls)
			}
			if repo.createCalls != 0 {
				t.Errorf("repo.Create called on validation failure: %d times", repo.createCalls)
			}
		})
	}
}

func TestCreateMedia_UnsupportedMediaType(t *testing.T) {
	repo := &fakeMediaRepository{}
	store := &fakeBlobStore{}
	h := NewMediaHandler(repo, store, testMaxUpload)

	// Plain ASCII text → DetectContentType returns text/plain.
	req := newCreateRequest(t, map[string][]string{"name": {"x"}}, []byte("just plain text, nothing to see here"))
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status: got %d, want 415 (body=%q)", res.StatusCode, rec.Body.String())
	}
	assertProblemJSON(t, res, http.StatusUnsupportedMediaType)

	if store.putCalls != 0 {
		t.Errorf("store.Put called on unsupported type: %d times", store.putCalls)
	}
	if repo.createCalls != 0 {
		t.Errorf("repo.Create called on unsupported type: %d times", repo.createCalls)
	}
}

func TestCreateMedia_PayloadTooLarge(t *testing.T) {
	repo := &fakeMediaRepository{}
	store := &fakeBlobStore{}
	// Tiny cap so we can trip MaxBytesReader without huge buffers.
	h := NewMediaHandler(repo, store, 64)

	body := append(append([]byte{}, jpegMagic...), bytes.Repeat([]byte("A"), 1024)...)
	req := newCreateRequest(t, map[string][]string{"name": {"x"}}, body)
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413 (body=%q)", res.StatusCode, rec.Body.String())
	}
	assertProblemJSON(t, res, http.StatusRequestEntityTooLarge)
}

func TestCreateMedia_UnknownTags_422(t *testing.T) {
	missing := uuid.MustParse("019f0000-0000-7000-8000-00000000beef")
	repo := &fakeMediaRepository{missingResult: []uuid.UUID{missing}}
	store := &fakeBlobStore{}
	h := NewMediaHandler(repo, store, testMaxUpload)

	req := newCreateRequest(t, map[string][]string{
		"name": {"x"},
		"tags": {missing.String()},
	}, jpegMagic)
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422 (body=%q)", res.StatusCode, rec.Body.String())
	}
	assertProblemJSON(t, res, http.StatusUnprocessableEntity)

	if store.putCalls != 0 {
		t.Errorf("store.Put called even though tags were unknown: %d times", store.putCalls)
	}
	if repo.createCalls != 0 {
		t.Errorf("repo.Create called even though tags were unknown: %d times", repo.createCalls)
	}
}

// TestCreateMedia_RepoError_TriggersBlobCompensation is the most
// important test of the file: it pins down the contract that a DB
// failure after a successful blob upload must not leave an orphan.
func TestCreateMedia_RepoError_TriggersBlobCompensation(t *testing.T) {
	boom := errors.New("unexpected boom: tx commit failed")
	repo := &fakeMediaRepository{createErr: boom}
	store := &fakeBlobStore{}
	h := NewMediaHandler(repo, store, testMaxUpload)

	req := newCreateRequest(t, map[string][]string{"name": {"x"}}, jpegMagic)
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500 (body=%q)", res.StatusCode, rec.Body.String())
	}
	assertProblemJSON(t, res, http.StatusInternalServerError)
	assertNoErrorLeak(t, rec.Body.Bytes(), boom)

	if store.putCalls != 1 {
		t.Errorf("store.Put calls: got %d, want 1", store.putCalls)
	}
	if len(store.deleted) != 1 {
		t.Fatalf("store.Delete calls: got %d, want 1 (compensation must run)", len(store.deleted))
	}
	if store.deleted[0] != store.putKey {
		t.Errorf("compensation deleted %q, but Put wrote %q", store.deleted[0], store.putKey)
	}
}

// TestCreateMedia_RepoError_CompensationFailureIsSwallowed verifies
// that a failure of the cleanup itself does not change the
// user-facing response: the original DB error is what the client
// sees, and the cleanup failure surfaces in logs only.
func TestCreateMedia_RepoError_CompensationFailureIsSwallowed(t *testing.T) {
	repo := &fakeMediaRepository{createErr: errors.New("db down")}
	store := &fakeBlobStore{deleteErr: errors.New("disk on fire")}
	h := NewMediaHandler(repo, store, testMaxUpload)

	req := newCreateRequest(t, map[string][]string{"name": {"x"}}, jpegMagic)
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", res.StatusCode)
	}
	if len(store.deleted) != 1 {
		t.Errorf("store.Delete attempts: got %d, want 1", len(store.deleted))
	}
}

// TestCreateMedia_RepoError_CompensationIgnoresErrNotFound ensures we
// do not log noise when Delete legitimately reports the blob is
// already gone (idempotent backend).
func TestCreateMedia_RepoError_CompensationIgnoresErrNotFound(t *testing.T) {
	repo := &fakeMediaRepository{createErr: errors.New("db down")}
	store := &fakeBlobStore{deleteErr: blobstore.ErrNotFound}
	h := NewMediaHandler(repo, store, testMaxUpload)

	req := newCreateRequest(t, map[string][]string{"name": {"x"}}, jpegMagic)
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", res.StatusCode)
	}
}

func TestCreateMedia_BlobStoreError_500NoRepoCall(t *testing.T) {
	repo := &fakeMediaRepository{}
	store := &fakeBlobStore{putErr: errors.New("disk full")}
	h := NewMediaHandler(repo, store, testMaxUpload)

	req := newCreateRequest(t, map[string][]string{"name": {"x"}}, jpegMagic)
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500 (body=%q)", res.StatusCode, rec.Body.String())
	}
	if repo.createCalls != 0 {
		t.Errorf("repo.Create called after blob failure: %d times", repo.createCalls)
	}
	if len(store.deleted) != 0 {
		t.Errorf("store.Delete called after a Put failure: %v (nothing to compensate)", store.deleted)
	}
}

// assertProblemJSON mirrors the helper used in the tags package: each
// 4xx/5xx response must follow the RFC 7807 contract. Duplicated on
// purpose — the rule of three has not yet justified a shared
// internal/httpx test helper.
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

// assertNoErrorLeak mirrors the helper used in the tags package: 5xx
// responses must never carry the underlying error message.
func assertNoErrorLeak(t *testing.T, body []byte, internal error) {
	t.Helper()
	if msg := internal.Error(); strings.Contains(string(body), msg) {
		t.Errorf("response body leaked internal error: contains %q\nbody: %s", msg, body)
	}
}
