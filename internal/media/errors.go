package media

import "errors"

// ErrMediaNotFound is returned by the repository when a media id does
// not exist.
var ErrMediaNotFound = errors.New("media not found")

// ErrUnknownTags is returned by the create flow when the caller
// references one or more tag ids that are not present in the `tags`
// table.
var ErrUnknownTags = errors.New("unknown tag ids")
