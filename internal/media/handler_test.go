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

	getMedia Media
	getTags  []Tag
	getErr   error
	getCalls int
	getID    uuid.UUID

	getMetadataMedia Media
	getMetadataErr   error
	getMetadataCalls int
	getMetadataID    uuid.UUID
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

func (f *fakeMediaRepository) Get(_ context.Context, id uuid.UUID) (Media, []Tag, error) {
	f.getCalls++
	f.getID = id
	return f.getMedia, f.getTags, f.getErr
}

func (f *fakeMediaRepository) GetMetadata(_ context.Context, id uuid.UUID) (Media, error) {
	f.getMetadataCalls++
	f.getMetadataID = id
	return f.getMetadataMedia, f.getMetadataErr
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
	// deleteCtxErr / deleteCtxHasDeadline snapshot the cleanup ctx at
	// call time. Capturing the *Context itself would race with the
	// caller's defer cancel() once createMedia returns, so we read
	// the bits we care about while we are still inside Delete.
	deleteCtxErr         error
	deleteCtxHasDeadline bool

	openBytes []byte
	openErr   error
	openCalls int
	openKey   string
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

// nopCloser wraps a *bytes.Reader so it satisfies io.ReadSeekCloser.
// io.NopCloser does not work here because it returns io.ReadCloser
// without Seek, which http.ServeContent needs for Range requests.
type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

func (f *fakeBlobStore) Open(_ context.Context, key string) (io.ReadSeekCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls++
	f.openKey = key
	if f.openErr != nil {
		return nil, f.openErr
	}
	return nopCloser{bytes.NewReader(f.openBytes)}, nil
}

func (f *fakeBlobStore) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, key)
	f.deleteCtxErr = ctx.Err()
	_, f.deleteCtxHasDeadline = ctx.Deadline()
	return f.deleteErr
}

// buildMultipart assembles a multipart/form-data body. The returned
// content-type carries the boundary, which the handler needs to parse
// the body. Pass fileBody=nil to omit the file part entirely.
func buildMultipart(t *testing.T, fields map[string][]string, fileField, fileName string, fileBody []byte) (body *bytes.Buffer, contentType string) {
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
	h := NewHandler(repo, store, testMaxUpload)

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
	h := NewHandler(repo, store, testMaxUpload)

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
		name     string
		fields   map[string][]string
		fileBody []byte
		wantSubs string
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
			h := NewHandler(repo, store, testMaxUpload)

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
	h := NewHandler(repo, store, testMaxUpload)

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
	h := NewHandler(repo, store, 64)

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
	h := NewHandler(repo, store, testMaxUpload)

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
	h := NewHandler(repo, store, testMaxUpload)

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
	h := NewHandler(repo, store, testMaxUpload)

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
	h := NewHandler(repo, store, testMaxUpload)

	req := newCreateRequest(t, map[string][]string{"name": {"x"}}, jpegMagic)
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", res.StatusCode)
	}
}

// TestCreateMedia_RepoError_CompensationCtxDetachedFromRequest pins
// the contract that the cleanup Delete runs on a context detached
// from the request: a client disconnect mid-rollback must not abort
// the compensation and turn a recoverable failure into an orphan
// blob. Today fsstore ignores ctx, but DESIGN.md §1 promises the path
// to S3 is "a new package implementing the same interface" — and an
// S3 implementation will honor ctx. Without this guarantee, every
// client disconnect on a failing POST /media would leak a blob.
func TestCreateMedia_RepoError_CompensationCtxDetachedFromRequest(t *testing.T) {
	repo := &fakeMediaRepository{createErr: errors.New("db down")}
	store := &fakeBlobStore{}
	h := NewHandler(repo, store, testMaxUpload)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: simulates a client that disconnected.

	req := newCreateRequest(t, map[string][]string{"name": {"x"}}, jpegMagic).WithContext(cancelledCtx)
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	if len(store.deleted) != 1 {
		t.Fatalf("store.Delete calls: got %d, want 1 (compensation must run even on cancelled request)", len(store.deleted))
	}
	if store.deleteCtxErr != nil {
		t.Errorf("compensation ctx was cancelled at call time (%v); it must be detached from the request ctx", store.deleteCtxErr)
	}
	if !store.deleteCtxHasDeadline {
		t.Error("compensation ctx has no deadline; it must be time-bounded")
	}
}

func TestCreateMedia_BlobStoreError_500NoRepoCall(t *testing.T) {
	repo := &fakeMediaRepository{}
	store := &fakeBlobStore{putErr: errors.New("disk full")}
	h := NewHandler(repo, store, testMaxUpload)

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

// newGetRequest builds a GET /media/{id} request with the path
// parameter pre-populated. We bypass the router on purpose: handler
// unit tests should not depend on routing wiring, only on the
// handler's own logic. The route is exercised end-to-end in
// integration tests and through a real mux in cmd/api.
func newGetRequest(t *testing.T, id string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/media/"+id, http.NoBody)
	req.SetPathValue("id", id)
	return req
}

func TestGetMedia_OK(t *testing.T) {
	mediaID := uuid.MustParse("019f0000-0000-7000-8000-0000000000aa")
	tagA := Tag{ID: uuid.MustParse("019f0000-0000-7000-8000-000000000001"), Name: "Messi"}
	tagB := Tag{ID: uuid.MustParse("019f0000-0000-7000-8000-000000000002"), Name: "Real Madrid"}

	repo := &fakeMediaRepository{
		getMedia: Media{ID: mediaID, Name: "Messi goal", FileKey: mediaID.String(), ContentType: "image/jpeg"},
		getTags:  []Tag{tagA, tagB},
	}
	h := NewHandler(repo, &fakeBlobStore{}, testMaxUpload)

	rec := httptest.NewRecorder()
	h.Get(rec, newGetRequest(t, mediaID.String()))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%q)", res.StatusCode, rec.Body.String())
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type: got %q, want application/json*", ct)
	}

	var got getMediaResponse
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != mediaID {
		t.Errorf("response id: got %s, want %s", got.ID, mediaID)
	}
	if got.Name != "Messi goal" {
		t.Errorf("response name: got %q, want %q", got.Name, "Messi goal")
	}
	if got.FileURL != "/media/"+mediaID.String()+"/file" {
		t.Errorf("response fileUrl: got %q, want %q", got.FileURL, "/media/"+mediaID.String()+"/file")
	}
	if len(got.Tags) != 2 || got.Tags[0] != tagA || got.Tags[1] != tagB {
		t.Errorf("response tags: got %+v, want [%+v %+v]", got.Tags, tagA, tagB)
	}

	if repo.getCalls != 1 {
		t.Errorf("repo.Get calls: got %d, want 1", repo.getCalls)
	}
	if repo.getID != mediaID {
		t.Errorf("repo received id %s, want %s", repo.getID, mediaID)
	}
}

func TestGetMedia_EmptyTagsReturnsEmptyArray(t *testing.T) {
	mediaID := uuid.MustParse("019f0000-0000-7000-8000-0000000000bb")

	// repo.getTags is left nil on purpose: the handler must coerce
	// nil to [] so the JSON contract stays "tags is always an
	// array, never null".
	repo := &fakeMediaRepository{
		getMedia: Media{ID: mediaID, Name: "untagged", FileKey: mediaID.String(), ContentType: "image/jpeg"},
	}
	h := NewHandler(repo, &fakeBlobStore{}, testMaxUpload)

	rec := httptest.NewRecorder()
	h.Get(rec, newGetRequest(t, mediaID.String()))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", res.StatusCode)
	}

	// Re-decode through a typed struct so a missing JSON field is
	// caught as a regression. Then confirm the on-the-wire payload
	// has [] (not null) by also inspecting the raw bytes.
	var got getMediaResponse
	body := rec.Body.Bytes()
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tags == nil || len(got.Tags) != 0 {
		t.Errorf("response tags: got %v, want []", got.Tags)
	}
	if !strings.Contains(string(body), `"tags":[]`) {
		t.Errorf("raw body must serialise tags as [], got: %s", body)
	}
}

func TestGetMedia_MalformedID_400(t *testing.T) {
	repo := &fakeMediaRepository{}
	h := NewHandler(repo, &fakeBlobStore{}, testMaxUpload)

	rec := httptest.NewRecorder()
	h.Get(rec, newGetRequest(t, "not-a-uuid"))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (body=%q)", res.StatusCode, rec.Body.String())
	}
	assertProblemJSON(t, res, http.StatusBadRequest)

	if repo.getCalls != 0 {
		t.Errorf("repo.Get calls: got %d, want 0 (handler must reject before reaching repo)", repo.getCalls)
	}
}

func TestGetMedia_NotFound_404(t *testing.T) {
	repo := &fakeMediaRepository{getErr: ErrMediaNotFound}
	h := NewHandler(repo, &fakeBlobStore{}, testMaxUpload)

	rec := httptest.NewRecorder()
	h.Get(rec, newGetRequest(t, uuid.NewString()))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404 (body=%q)", res.StatusCode, rec.Body.String())
	}
	assertProblemJSON(t, res, http.StatusNotFound)
}

func TestGetMedia_InternalError(t *testing.T) {
	boom := errors.New("unexpected boom: pgx unreachable")
	repo := &fakeMediaRepository{getErr: boom}
	h := NewHandler(repo, &fakeBlobStore{}, testMaxUpload)

	rec := httptest.NewRecorder()
	h.Get(rec, newGetRequest(t, uuid.NewString()))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", res.StatusCode)
	}
	assertProblemJSON(t, res, http.StatusInternalServerError)
	assertNoErrorLeak(t, rec.Body.Bytes(), boom)
}

// newServeFileRequest mirrors newGetRequest: bypass the router and
// inject the path value directly so the unit test exercises only
// the handler's logic.
func newServeFileRequest(t *testing.T, id string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/media/"+id+"/file", http.NoBody)
	req.SetPathValue("id", id)
	return req
}

func TestServeFile_OK(t *testing.T) {
	mediaID := uuid.MustParse("019f0000-0000-7000-8000-0000000000cc")
	body := append(append([]byte{}, jpegMagic...), bytes.Repeat([]byte("Y"), 256)...)

	repo := &fakeMediaRepository{
		getMetadataMedia: Media{
			ID:          mediaID,
			Name:        "Messi goal",
			FileKey:     mediaID.String(),
			ContentType: "image/jpeg",
		},
	}
	store := &fakeBlobStore{openBytes: body}
	h := NewHandler(repo, store, testMaxUpload)

	rec := httptest.NewRecorder()
	h.ServeFile(rec, newServeFileRequest(t, mediaID.String()))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%q)", res.StatusCode, rec.Body.String())
	}
	if ct := res.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type: got %q, want image/jpeg (must come from DB, not be sniffed)", ct)
	}
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body bytes: got %d, want %d", len(got), len(body))
	}

	if repo.getMetadataCalls != 1 {
		t.Errorf("repo.GetMetadata calls: got %d, want 1", repo.getMetadataCalls)
	}
	if repo.getCalls != 0 {
		t.Errorf("repo.Get calls: got %d, want 0 (binary endpoint must use the lighter GetMetadata)", repo.getCalls)
	}
	if store.openKey != mediaID.String() {
		t.Errorf("store.Open key: got %q, want %q", store.openKey, mediaID.String())
	}
}

func TestServeFile_MalformedID_400(t *testing.T) {
	repo := &fakeMediaRepository{}
	store := &fakeBlobStore{}
	h := NewHandler(repo, store, testMaxUpload)

	rec := httptest.NewRecorder()
	h.ServeFile(rec, newServeFileRequest(t, "not-a-uuid"))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (body=%q)", res.StatusCode, rec.Body.String())
	}
	assertProblemJSON(t, res, http.StatusBadRequest)
	if repo.getMetadataCalls != 0 {
		t.Errorf("repo.GetMetadata called on malformed id: %d times", repo.getMetadataCalls)
	}
	if store.openCalls != 0 {
		t.Errorf("store.Open called on malformed id: %d times", store.openCalls)
	}
}

func TestServeFile_MediaNotFound_404(t *testing.T) {
	repo := &fakeMediaRepository{getMetadataErr: ErrMediaNotFound}
	store := &fakeBlobStore{}
	h := NewHandler(repo, store, testMaxUpload)

	rec := httptest.NewRecorder()
	h.ServeFile(rec, newServeFileRequest(t, uuid.NewString()))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", res.StatusCode)
	}
	assertProblemJSON(t, res, http.StatusNotFound)
	if store.openCalls != 0 {
		t.Errorf("store.Open called when media row was missing: %d times", store.openCalls)
	}
}

// TestServeFile_BlobMissing_404 covers the "row exists, blob does
// not" corruption case. The blob store contract returns ErrNotFound
// in that scenario; the handler turns it into a 404 (not a 500): the
// client cannot do anything with this distinction, but the operator
// gets a structured log line with the offending file_key.
func TestServeFile_BlobMissing_404(t *testing.T) {
	mediaID := uuid.MustParse("019f0000-0000-7000-8000-0000000000dd")
	repo := &fakeMediaRepository{
		getMetadataMedia: Media{ID: mediaID, FileKey: mediaID.String(), ContentType: "image/jpeg"},
	}
	store := &fakeBlobStore{openErr: blobstore.ErrNotFound}
	h := NewHandler(repo, store, testMaxUpload)

	rec := httptest.NewRecorder()
	h.ServeFile(rec, newServeFileRequest(t, mediaID.String()))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404 (body=%q)", res.StatusCode, rec.Body.String())
	}
	assertProblemJSON(t, res, http.StatusNotFound)
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
