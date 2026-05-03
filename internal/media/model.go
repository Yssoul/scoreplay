package media

import "github.com/google/uuid"

// Media is the domain representation of an uploaded asset.
type Media struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	FileKey     string    `json:"-"`
	ContentType string    `json:"-"`
}

// Tag is the subset of the tag entity that media handlers and DTOs
// need. It is intentionally a copy of tags.Tag rather than an import,
// to keep the media package free of inter-domain dependencies.
type Tag struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}
