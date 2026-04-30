// Package httpx provides small, framework-free helpers built on top of net/http:
// a RFC 7807 problem type, JSON response helpers, and error translation.
package httpx

// Problem is the RFC 7807 (application/problem+json) representation of an
// error response. Only fields actually used are exposed.
//
// Reference: https://www.rfc-editor.org/rfc/rfc7807
type Problem struct {
	Title string `json:"title"`
	// Status mirrors the HTTP status code for convenience of the client.
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}
