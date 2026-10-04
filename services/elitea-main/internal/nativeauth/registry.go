package nativeauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/clientversion"
)

// NativeClientsPathEnv names the file layer of the client registry: a JSON or
// YAML list of {client_id, display_name, redirect_uris, enabled}.
const NativeClientsPathEnv = "NATIVE_CLIENTS_PATH"

// The two layers a client can come from. A DB row overrides a file entry with
// the same client_id — the precedence ADR-0024 uses for branding.
const (
	SourceFile = "file"
	SourceDB   = "db"
)

// registryCacheTTL bounds how stale a replica's view of the DB layer can be.
// An admin save invalidates the local replica immediately.
const registryCacheTTL = 15 * time.Second

var clientIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

// Client is one registered native public client.
type Client struct {
	ClientID     string   `json:"client_id"`
	DisplayName  string   `json:"display_name"`
	RedirectURIs []string `json:"redirect_uris"`
	Enabled      bool     `json:"enabled"`
	// MinClientVersion is this client's own minimum version (ADR-0025 WP4),
	// "" for none. It can only RAISE the deployment-wide minimum of the
	// native_client_policy section, never lower it: the effective minimum is
	// the higher of the two (internal/application/nativepolicy).
	MinClientVersion string `json:"min_client_version"`
	// Source is SourceFile or SourceDB.
	Source string `json:"source"`
	// OverriddenFile is true on a DB row that shadows a file entry.
	OverriddenFile bool `json:"overridden_file"`
}

// ClientValidationError lists every reason a client definition was refused,
// keyed by field ("client_id", "display_name", "redirect_uris[0]", ...).
type ClientValidationError struct {
	Reasons map[string]string
}

func (e *ClientValidationError) Error() string {
	keys := make([]string, 0, len(e.Reasons))
	for key := range e.Reasons {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+": "+e.Reasons[key])
	}
	return "invalid native client: " + strings.Join(parts, "; ")
}

// ValidateClient checks a definition from either layer.
func ValidateClient(client Client) error {
	reasons := map[string]string{}
	if !clientIDPattern.MatchString(client.ClientID) {
		reasons["client_id"] = "3 to 64 characters: lower-case letters, digits, '.', '_' or '-', " +
			"starting with a letter or digit"
	}
	name := strings.TrimSpace(client.DisplayName)
	if name == "" || utf8.RuneCountInString(name) > 64 || len(name) > 256 ||
		strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		reasons["display_name"] = "1 to 64 characters without control characters"
	}
	switch {
	case len(client.RedirectURIs) == 0:
		reasons["redirect_uris"] = "at least one redirect URI is required"
	case len(client.RedirectURIs) > MaxRedirectURIs:
		reasons["redirect_uris"] = fmt.Sprintf("at most %d redirect URIs", MaxRedirectURIs)
	default:
		seen := map[string]bool{}
		for index, uri := range client.RedirectURIs {
			key := fmt.Sprintf("redirect_uris[%d]", index)
			if _, err := ValidateRedirectURI(uri); err != nil {
				reasons[key] = RedirectReason(err)
				continue
			}
			if seen[uri] {
				reasons[key] = "duplicate redirect URI"
			}
			seen[uri] = true
		}
	}
	if client.MinClientVersion != "" && !clientversion.Valid(client.MinClientVersion) {
		reasons["min_client_version"] = "empty, or a version MAJOR.MINOR.PATCH with an optional -prerelease"
	}
	if len(reasons) > 0 {
		return &ClientValidationError{Reasons: reasons}
	}
	return nil
}

type fileClient struct {
	ClientID     string   `yaml:"client_id"`
	DisplayName  string   `yaml:"display_name"`
	RedirectURIs []string `yaml:"redirect_uris"`
	Enabled      *bool    `yaml:"enabled"`
	// MinClientVersion is optional (ADR-0025 WP4).
	MinClientVersion string `yaml:"min_client_version"`
}

// ParseClientsFile parses the file layer. JSON is valid YAML, so one decoder
// reads both. Unknown keys, a duplicate client_id and any invalid entry are
// errors: a file that does not parse refuses boot.
func ParseClientsFile(content []byte) ([]Client, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var entries []fileClient
	if err := decoder.Decode(&entries); err != nil {
		return nil, fmt.Errorf("parse native clients file: %w", err)
	}
	clients := make([]Client, 0, len(entries))
	seen := map[string]bool{}
	for index, entry := range entries {
		client := Client{
			ClientID:         entry.ClientID,
			DisplayName:      strings.TrimSpace(entry.DisplayName),
			RedirectURIs:     entry.RedirectURIs,
			Enabled:          entry.Enabled == nil || *entry.Enabled,
			MinClientVersion: strings.TrimSpace(entry.MinClientVersion),
			Source:           SourceFile,
		}
		if err := ValidateClient(client); err != nil {
			return nil, fmt.Errorf("native clients file entry %d: %w", index, err)
		}
		if seen[client.ClientID] {
			return nil, fmt.Errorf("native clients file: client_id %q appears twice", client.ClientID)
		}
		seen[client.ClientID] = true
		clients = append(clients, client)
	}
	return clients, nil
}

// LoadClientsFile reads NATIVE_CLIENTS_PATH. An empty path is no file layer.
func LoadClientsFile(path string) ([]Client, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	content, err := os.ReadFile(path) //nolint:gosec // operator-supplied deployment configuration path
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", NativeClientsPathEnv, err)
	}
	return ParseClientsFile(content)
}

// Registry is the effective client registry: the file layer overlaid by the DB
// layer, resolved per request through a short cache.
type Registry struct {
	file []Client
	pool *pgxpool.Pool
	now  func() time.Time

	mu       sync.Mutex
	cached   []Client
	cachedAt time.Time
	valid    bool
}

// NewRegistry builds the registry. pool may be nil (file layer only).
func NewRegistry(file []Client, pool *pgxpool.Pool) *Registry {
	return &Registry{file: append([]Client(nil), file...), pool: pool, now: time.Now}
}

// Invalidate drops the cached view; the next read goes to the database.
func (r *Registry) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.valid = false
	r.mu.Unlock()
}

// Clients is the effective list, sorted by client_id.
func (r *Registry) Clients(ctx context.Context) ([]Client, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	if r.valid && r.now().Sub(r.cachedAt) < registryCacheTTL {
		out := cloneClients(r.cached)
		r.mu.Unlock()
		return out, nil
	}
	r.mu.Unlock()

	merged, err := r.load(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cached, r.cachedAt, r.valid = merged, r.now(), true
	r.mu.Unlock()
	return cloneClients(merged), nil
}

func (r *Registry) load(ctx context.Context) ([]Client, error) {
	byID := map[string]Client{}
	for _, client := range r.file {
		byID[client.ClientID] = client
	}
	if r.pool != nil {
		rows, err := r.pool.Query(ctx, `
			SELECT client_id, display_name, redirect_uris, enabled, min_client_version
			FROM elitea_auth.native_clients`)
		if err != nil {
			return nil, fmt.Errorf("nativeauth: read native clients: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var client Client
			if err := rows.Scan(&client.ClientID, &client.DisplayName, &client.RedirectURIs, &client.Enabled,
				&client.MinClientVersion); err != nil {
				return nil, fmt.Errorf("nativeauth: scan native client: %w", err)
			}
			client.Source = SourceDB
			_, client.OverriddenFile = byID[client.ClientID]
			byID[client.ClientID] = client
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("nativeauth: read native clients: %w", err)
		}
	}
	merged := make([]Client, 0, len(byID))
	for _, client := range byID {
		merged = append(merged, client)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ClientID < merged[j].ClientID })
	return merged, nil
}

// Lookup returns the effective definition of one client, enabled or not.
func (r *Registry) Lookup(ctx context.Context, clientID string) (Client, bool, error) {
	clients, err := r.Clients(ctx)
	if err != nil {
		return Client{}, false, err
	}
	for _, client := range clients {
		if client.ClientID == clientID {
			return client, true, nil
		}
	}
	return Client{}, false, nil
}

// Active reports whether clientID is registered and enabled.
func (r *Registry) Active(ctx context.Context, clientID string) (Client, bool, error) {
	client, ok, err := r.Lookup(ctx, clientID)
	if err != nil || !ok || !client.Enabled {
		return Client{}, false, err
	}
	return client, true, nil
}

// Registered reports whether ANY client exists in either layer, enabled or
// not. While none does, every /api/v2/auth/native/* route answers 404 (the
// ADR's "no client, no endpoints").
func (r *Registry) Registered(ctx context.Context) (bool, error) {
	clients, err := r.Clients(ctx)
	return len(clients) > 0, err
}

// AnyEnabled reports whether at least one client is enabled: what discovery's
// `native_auth` (null or the endpoints) turns on.
func (r *Registry) AnyEnabled(ctx context.Context) (bool, error) {
	clients, err := r.Clients(ctx)
	if err != nil {
		return false, err
	}
	for _, client := range clients {
		if client.Enabled {
			return true, nil
		}
	}
	return false, nil
}

// FileClients is the file layer as loaded at boot.
func (r *Registry) FileClients() []Client {
	if r == nil {
		return nil
	}
	return cloneClients(r.file)
}

func cloneClients(in []Client) []Client {
	out := make([]Client, len(in))
	for i, client := range in {
		client.RedirectURIs = append([]string(nil), client.RedirectURIs...)
		out[i] = client
	}
	return out
}

// ErrClientNotFound is a DB-layer delete of a client_id with no row.
var ErrClientNotFound = errors.New("native client not found")
