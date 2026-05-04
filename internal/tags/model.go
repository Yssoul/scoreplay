// Package tags implements the tag bounded context: domain types,
// repository, and HTTP handlers backing POST /tags and GET /tags.
package tags

import "github.com/google/uuid"

// Tag is the domain representation of a tag, used both as the
// repository and the HTTP DTO.
type Tag struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}
