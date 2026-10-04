package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Tombstone is one removal in a `changes_since` delta (ADR-0025 WP6).
type Tombstone struct {
	ID        int64   `json:"id"`
	UUID      *string `json:"uuid"`
	Reason    string  `json:"reason"`
	DeletedAt string  `json:"deleted_at"`
}

// DeltaPage is one page of a delta listing. The conversation and notification
// lists carry their rows in `rows`; the message list in `items` (its legacy
// key, kept by the delta shape).
type DeltaPage struct {
	Total      int64             `json:"total"`
	Rows       []json.RawMessage `json:"rows"`
	Items      []json.RawMessage `json:"items"`
	Tombstones []Tombstone       `json:"tombstones"`
	NextCursor string            `json:"next_cursor"`
	HasMore    bool              `json:"has_more"`
}

// SyncResult is a delta drained to the end.
type SyncResult struct {
	Rows       []map[string]any
	Tombstones []Tombstone
	Cursor     string
	Pages      int
}

// SyncError is a refused delta request.
type SyncError struct {
	Response *Response
	Code     string
}

func (e *SyncError) Error() string { return fmt.Sprintf("delta refused (%s): %s", e.Code, e.Response) }

// maxSyncPages bounds a drain: a server that answers has_more forever with a
// cursor that does not move must fail the run, not hang it.
const maxSyncPages = 200

// Sync drains a `changes_since` listing from cursor ("0" is a full sync),
// following `has_more` until the server says the list is exhausted. path may
// already carry a query; `changes_since` (and `limit` when > 0) is added.
func (c *Client) Sync(ctx context.Context, path, cursor string, limit int) (SyncResult, error) {
	result := SyncResult{Cursor: cursor}
	for {
		if result.Pages >= maxSyncPages {
			return result, fmt.Errorf("delta of %s did not end after %d pages", path, maxSyncPages)
		}
		page, err := c.deltaPage(ctx, path, result.Cursor, limit)
		if err != nil {
			return result, err
		}
		result.Pages++
		for _, raw := range page.Rows {
			var row map[string]any
			if err := json.Unmarshal(raw, &row); err != nil {
				return result, fmt.Errorf("delta row: %w", err)
			}
			result.Rows = append(result.Rows, row)
		}
		result.Tombstones = append(result.Tombstones, page.Tombstones...)
		if page.NextCursor == "" {
			return result, fmt.Errorf("delta page of %s carries no next_cursor", path)
		}
		previous := result.Cursor
		result.Cursor = page.NextCursor
		if !page.HasMore {
			return result, nil
		}
		if page.NextCursor == previous {
			return result, fmt.Errorf("delta of %s said has_more without moving its cursor", path)
		}
	}
}

func (c *Client) deltaPage(ctx context.Context, path, cursor string, limit int) (DeltaPage, error) {
	parsed, err := url.Parse(path)
	if err != nil {
		return DeltaPage{}, err
	}
	query := parsed.Query()
	query.Set("changes_since", cursor)
	if limit > 0 {
		query.Set("limit", fmt.Sprint(limit))
	}
	parsed.RawQuery = query.Encode()
	response, err := c.Do(ctx, http.MethodGet, parsed.String(), nil, nil)
	if err != nil {
		return DeltaPage{}, err
	}
	if response.Status != http.StatusOK {
		return DeltaPage{}, &SyncError{Response: response, Code: errorCode(response)}
	}
	var page DeltaPage
	if err := response.JSON(&page); err != nil {
		return DeltaPage{}, err
	}
	if page.Rows == nil {
		page.Rows = page.Items
	}
	if page.Rows == nil {
		return DeltaPage{}, fmt.Errorf("delta page carries neither a rows nor an items array: %s", response)
	}
	return page, nil
}

// RowID reads a row's integer id whatever JSON type the list uses for it.
func RowID(row map[string]any) string {
	switch value := row["id"].(type) {
	case float64:
		return fmt.Sprintf("%.0f", value)
	case string:
		return value
	case json.Number:
		return value.String()
	default:
		return ""
	}
}

// RowsByID indexes rows by id; a later row replaces an earlier one, which is
// the upsert rule the settle window requires.
func RowsByID(rows []map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		if id := RowID(row); id != "" {
			out[id] = row
		}
	}
	return out
}

// TombstoneFor finds the tombstone of id, if any.
func TombstoneFor(tombstones []Tombstone, id string) (Tombstone, bool) {
	for _, tombstone := range tombstones {
		if fmt.Sprint(tombstone.ID) == strings.TrimSpace(id) {
			return tombstone, true
		}
	}
	return Tombstone{}, false
}
