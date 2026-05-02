package tags

import "errors"

// ErrNameConflict is returned when a tag with the same name (case-insensitive)
// already exists. Tag names are unique across the system; this invariant is
// enforced by the storage layer (UNIQUE constraint on tags.name with CITEXT).
var ErrNameConflict = errors.New("tag name already exists")
