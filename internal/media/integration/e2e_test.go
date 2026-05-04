//go:build integration

// This file holds the end-to-end suite for the POST /media endpoint.
// It is deliberately small.
//
// The handler's branch coverage (validation, sniff, compensation,
// status mapping) lives in internal/media/handler_test.go where it
// runs in milliseconds without Docker. Re-running those branches
// against a real Postgres + fsstore would only duplicate
// assertions and pay for it in container startup time.
//
// What stays here is what unit tests cannot prove:
//
//   - HappyPath: a single smoke that wires HTTP → handler → repo →
//     pgxpool → fsstore end-to-end. It is the only line of defense
//     against a wiring regression in cmd/api/main.go (a swapped
//     dependency, a missing route registration) that every unit
//     test would still pass.
//   - FKConsistency_TagDeleteRestricted: enforces a *database*
//     contract (ON DELETE RESTRICT, DESIGN.md §3.7) that no fake
//     can reproduce.
package mediaintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ygalmessas/scoreplay/internal/blobstore/fsstore"
	"github.com/ygalmessas/scoreplay/internal/media"
)

// jpegMagic is the SOI marker JPEG decoders look for. Three bytes are
// enough for http.DetectContentType to classify the upload as
// image/jpeg without us bundling a real image in the test sources.
var jpegMagic = []byte{0xFF, 0xD8, 0xFF}

// startMediaServer wires the production POST /media route against a
// real fsstore (rooted at t.TempDir()) and the live testcontainer
// pool. The returned URL is hit through net/http like a remote
// client would. testMaxUpload is generous enough to never trip 413
// in the happy paths.
const testMaxUpload = 10 << 20 // 10 MiB

func startMediaServer(t *testing.T, pool *pgxpool.Pool) (baseURL string, blobRoot string) {
	t.Helper()

	blobRoot = t.TempDir()
	store, err := fsstore.New(blobRoot)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}

	repo := media.NewPgRepository(pool)
	h := media.NewHandler(repo, store, testMaxUpload)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /media", h.Create)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv.URL, blobRoot
}

// seedTagWithName inserts a tag directly into Postgres and returns
// its id. Going through SQL (instead of the tags package) keeps this
// suite focused on the media slice and avoids cross-package test
// coupling.
func seedTagWithName(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tags (id, name, created_at) VALUES ($1, $2, $3)`,
		id, name, time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed tag %q: %v", name, err)
	}
	return id
}

// buildMultipart assembles a multipart/form-data body. fileBody=nil
// omits the file part entirely (used for the "missing file" cases,
// not exercised in this integration suite).
func buildMultipart(t *testing.T, fields map[string][]string, fileBody []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for k, vs := range fields {
		for _, v := range vs {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatalf("write field %s: %v", k, err)
			}
		}
	}
	if fileBody != nil {
		fw, err := mw.CreateFormFile("file", "asset.jpg")
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(fileBody); err != nil {
			t.Fatalf("write file body: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return body, mw.FormDataContentType()
}

func postMedia(t *testing.T, baseURL string, fields map[string][]string, fileBody []byte) *http.Response {
	t.Helper()
	body, ct := buildMultipart(t, fields, fileBody)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/media", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", ct)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /media: %v", err)
	}
	return res
}

// TestPOSTMedia_HappyPath_PersistsRowAttachmentsAndBlob is the test
// that actually proves the wiring works end-to-end: a multipart
// upload that should land a row in `media`, two rows in
// `media_tags`, and a file on disk under <blobRoot>/blobs/<id>. If
// any of these is missing, the contract is broken even though unit
// tests pass.
func TestPOSTMedia_HappyPath_PersistsRowAttachmentsAndBlob(t *testing.T) {
	pool := newTestPool(t)
	baseURL, blobRoot := startMediaServer(t, pool)

	tagA := seedTagWithName(t, pool, "Messi")
	tagB := seedTagWithName(t, pool, "Real Madrid")

	fileBody := append(append([]byte{}, jpegMagic...), bytes.Repeat([]byte("X"), 1024)...)

	res := postMedia(t, baseURL, map[string][]string{
		"name": {"Messi goal"},
		"tags": {tagA.String(), tagB.String()},
	}, fileBody)
	defer res.Body.Close()

	if res.StatusCode != http.StatusCreated {
		buf, _ := io.ReadAll(res.Body)
		t.Fatalf("status: got %d, want 201 (body=%q)", res.StatusCode, buf)
	}

	var got struct {
		ID      uuid.UUID   `json:"id"`
		Name    string      `json:"name"`
		Tags    []uuid.UUID `json:"tags"`
		FileURL string      `json:"fileUrl"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Name != "Messi goal" {
		t.Errorf("response name: got %q, want %q", got.Name, "Messi goal")
	}
	if got.FileURL != "/media/"+got.ID.String()+"/file" {
		t.Errorf("response fileUrl: got %q, want %q", got.FileURL, "/media/"+got.ID.String()+"/file")
	}
	if len(got.Tags) != 2 {
		t.Fatalf("response tags: got %v, want 2 ids", got.Tags)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		dbID          uuid.UUID
		dbName        string
		dbFileKey     string
		dbContentType string
	)
	err := pool.QueryRow(ctx,
		`SELECT id, name, file_key, content_type FROM media WHERE id = $1`,
		got.ID,
	).Scan(&dbID, &dbName, &dbFileKey, &dbContentType)
	if err != nil {
		t.Fatalf("select media: %v", err)
	}
	if dbName != "Messi goal" {
		t.Errorf("db name: got %q, want %q", dbName, "Messi goal")
	}
	if dbFileKey != got.ID.String() {
		t.Errorf("db file_key: got %q, want %q (file_key must equal media.id)", dbFileKey, got.ID.String())
	}
	if dbContentType != "image/jpeg" {
		t.Errorf("db content_type: got %q, want %q (sniff failed)", dbContentType, "image/jpeg")
	}

	var attachCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM media_tags WHERE media_id = $1`,
		got.ID,
	).Scan(&attachCount); err != nil {
		t.Fatalf("count media_tags: %v", err)
	}
	if attachCount != 2 {
		t.Errorf("media_tags rows: got %d, want 2", attachCount)
	}

	blobPath := filepath.Join(blobRoot, "blobs", got.ID.String())
	onDisk, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read blob from %s: %v", blobPath, err)
	}
	if !bytes.Equal(onDisk, fileBody) {
		t.Errorf("blob bytes: got %d on disk, want %d (content mismatch)", len(onDisk), len(fileBody))
	}
}

// TestPOSTMedia_FKConsistency_TagDeleteRestricted closes the loop on
// the ON DELETE RESTRICT choice for media_tags.tag_id (DESIGN.md
// §3.7): once a tag is attached to a media, the database refuses to
// drop it. This is a safety net against well-meaning operators
// running DELETE in psql.
func TestPOSTMedia_FKConsistency_TagDeleteRestricted(t *testing.T) {
	pool := newTestPool(t)
	baseURL, _ := startMediaServer(t, pool)

	tag := seedTagWithName(t, pool, "Pelé")

	res := postMedia(t, baseURL, map[string][]string{
		"name": {"goal of the century"},
		"tags": {tag.String()},
	}, jpegMagic)
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("setup: POST /media returned %d", res.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// We assert only that the DELETE is rejected; matching the
	// SQLSTATE precisely (23503 foreign_key_violation) would couple
	// the test to pgx internals. The presence of an error is enough
	// proof that ON DELETE RESTRICT is wired correctly.
	if _, err := pool.Exec(ctx, `DELETE FROM tags WHERE id = $1`, tag); err == nil {
		t.Fatal("delete tag: got nil error, want FK violation (RESTRICT must protect attached tags)")
	}
}
