// Package desktopwire holds the small wire helpers the desktop operation
// routes share (ADR-0029: localturns, desktopops): one route-id parser and one
// JSON answer shape, so the routes cannot drift apart on either.
package desktopwire

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
)

// PositiveID parses a route id: a canonical positive decimal that fits an
// INTEGER column ("007", "+7" and "1e3" are refused).
func PositiveID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0 && id <= math.MaxInt32 && strconv.FormatInt(id, 10) == raw
}

// WriteError answers {"error": code, "message": message}.
func WriteError(writer http.ResponseWriter, status int, code, message string) {
	WriteJSON(writer, status, map[string]string{"error": code, "message": message})
}

// WriteJSON answers value as JSON, never cached.
func WriteJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
