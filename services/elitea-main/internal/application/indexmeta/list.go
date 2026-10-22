// Package indexmeta holds the request, item and error vocabulary of the
// current index metadata routes. The index registry (application/indexregistry)
// serves them; HTTP authorization and route parsing remain transport concerns,
// and this package accepts only already-authorized numeric identities. The
// per-project PgVector reader it once held went with that store (#1359).
package indexmeta

import (
	"errors"
)

const (
	MaxCurrentIndexMetaRows    = 10_000
	MaxCurrentIndexMetaIDBytes = 4 << 10
)

var (
	ErrInvalidCurrentIndexMetaRequest = errors.New("invalid current index meta request")
	ErrCurrentIndexMetaToolkitMissing = errors.New("current index meta toolkit is not visible")
	ErrCurrentIndexMetaTargetMissing  = errors.New("current index meta storage target is unavailable")
	ErrCurrentIndexMetaUnavailable    = errors.New("current index meta is unavailable")
	ErrCurrentIndexMetaLimitExceeded  = errors.New("current index meta result exceeds its limit")
)

// Request contains only identities derived from the authenticated request.
// No storage target is accepted from HTTP input.
type Request struct {
	ProjectID   int64
	ActorUserID int64
	ToolkitID   int64
}

// Item is the exact current UI list element. A non-nil empty []Item serializes
// as [] rather than the incompatible {items,total} prototype response.
type Item struct {
	ID       string         `json:"id"`
	Metadata map[string]any `json:"metadata"`
	Stale    bool           `json:"stale"`
}
