//go:build integration

package tagsintegration

import (
	"context"
	"testing"
)

// TestNewTestPool_Smoke boots the integration fixture once and verifies a
// trivial query runs plus the tags table exists. It protects the helper
// itself from silent regression; the repository tests depend on it.
func TestNewTestPool_Smoke(t *testing.T) {
	pool := newTestPool(t)

	var one int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 returned %d", one)
	}

	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM tags").Scan(&count); err != nil {
		t.Fatalf("count(tags): %v (migrations probably did not run)", err)
	}
	if count != 0 {
		t.Fatalf("new cluster should have no tags, got %d", count)
	}
}
