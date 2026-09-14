package mcpregistry

// The PostgreSQL store behind `elitea_mcp.prebuilt_servers` (shared migration
// 0094).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrPrebuiltNotFound reports that no catalogue entry carries the key.
//
// It is distinct from a read failure. A caller that resolves a toolkit's
// settings treats "no such entry" as "nothing to inject" and carries on, and
// must not treat an unreadable table the same way: that would silently drop an
// operator's credentials into a request that then fails at the remote server
// with an authentication error naming nothing.
var ErrPrebuiltNotFound = errors.New("mcpregistry: no pre-built MCP server with that key")

// PrebuiltStore reads and writes the platform-wide pre-built MCP catalogue.
type PrebuiltStore struct {
	pool *pgxpool.Pool
}

func NewPrebuiltStore(pool *pgxpool.Pool) *PrebuiltStore { return &PrebuiltStore{pool: pool} }

const prebuiltColumns = `catalogue_key, display_name, server_url, base_url, client_id,
	client_secret_ref, timeout_seconds, headers, config_schema, enabled`

// List returns every catalogue entry, ordered by display name.
//
// The order is by the operator's own spelling rather than by the normalised
// key, because the admin page renders display names and an alphabetical listing
// that is alphabetical in a column nobody sees reads as unordered.
func (s *PrebuiltStore) List(ctx context.Context) ([]PrebuiltServer, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNoPool
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+prebuiltColumns+`
		   FROM elitea_mcp.prebuilt_servers
		  ORDER BY lower(display_name), catalogue_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]PrebuiltServer, 0)
	for rows.Next() {
		entry, err := scanPrebuilt(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// ListToolkitTypeSchemas returns enabled pre-built definitions for the toolkit form.
func (s *PrebuiltStore) ListToolkitTypeSchemas(ctx context.Context) (map[string]map[string]any, error) {
	entries, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string]map[string]any)
	for _, entry := range entries {
		if !entry.Enabled {
			continue
		}
		toolkitType, schema, err := prebuiltToolkitTypeSchema(entry)
		if err != nil {
			return nil, err
		}
		result[toolkitType] = schema
	}
	return result, nil
}

func prebuiltToolkitTypeSchema(entry PrebuiltServer) (string, map[string]any, error) {
	properties, err := PrebuiltConfigProperties(entry.ConfigSchema)
	if err != nil {
		return "", nil, err
	}
	properties["selected_tools"] = map[string]any{
		"type":         "array",
		"title":        "Selected Tools",
		"description":  "Optional remote tool names to enable. An empty list enables all tools.",
		"default":      []any{},
		"items":        map[string]any{"type": "string"},
		"args_schemas": map[string]any{},
	}
	required := make([]any, 0)
	for name, raw := range properties {
		property, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// The current static MCP contract marks sensitive fields with
		// `secret: true`. Main's project-vault writer follows JSON Schema and
		// recognizes `format: password`. Keep the source annotation for the
		// web client and add the standard format for the persistence boundary.
		if secret, _ := property["secret"].(bool); secret {
			property["format"] = "password"
		}
		if value, _ := property["required"].(bool); value {
			required = append(required, name)
		}
	}
	sort.Slice(required, func(i, j int) bool {
		return required[i].(string) < required[j].(string)
	})
	return PrebuiltToolkitTypePrefix + entry.Key, map[string]any{
		"type":          "object",
		"title":         entry.DisplayName,
		"name_required": true,
		"required":      required,
		"properties":    properties,
		"metadata": map[string]any{
			"label":       entry.DisplayName,
			"categories":  []any{"mcp"},
			"server_name": entry.Key,
			"server_type": "http",
		},
	}, nil
}

// Lookup resolves one toolkit type or catalogue name to its entry.
//
// The argument is normalised here rather than by the caller, so every call site
// matches on the same rule and a caller cannot accidentally look up a raw
// toolkit type against a normalised column.
//
// A DISABLED entry is returned, not hidden. Whether a disabled entry may be
// used is the resolver's decision (Resolve declines one), and an admin listing
// must be able to see it. Filtering here would make the two callers disagree
// about whether the row exists.
func (s *PrebuiltStore) Lookup(ctx context.Context, nameOrType string) (PrebuiltServer, error) {
	if s == nil || s.pool == nil {
		return PrebuiltServer{}, ErrNoPool
	}
	key := NormalizeCatalogueKey(nameOrType)
	if key == "" {
		return PrebuiltServer{}, ErrPrebuiltNotFound
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+prebuiltColumns+`
		   FROM elitea_mcp.prebuilt_servers
		  WHERE catalogue_key = $1`, key)
	entry, err := scanPrebuilt(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return PrebuiltServer{}, ErrPrebuiltNotFound
	}
	if err != nil {
		return PrebuiltServer{}, err
	}
	return entry, nil
}

// Upsert writes one catalogue entry, keyed by its normalised key.
//
// A write REPLACES the entry. That is pylon's semantics — its catalogue is a
// dictionary rebuilt from the descriptor on every reload — and it is what makes
// the admin page's save mean "this is the definition now" rather than "merge
// this into whatever was there".
//
// ClientSecretRef is written as given. Sealing the plaintext secret into the
// vault and producing the reference belongs to the admin write path, which owns
// the vault; this store must not acquire a second way to touch secrets.
func (s *PrebuiltStore) Upsert(ctx context.Context, entry PrebuiltServer) (PrebuiltServer, error) {
	if s == nil || s.pool == nil {
		return PrebuiltServer{}, ErrNoPool
	}

	display := strings.TrimSpace(entry.DisplayName)
	if display == "" {
		return PrebuiltServer{}, fmt.Errorf("mcpregistry: catalogue entry has no display name")
	}
	// The key is derived from the display name when the caller did not supply
	// one, which is what pylon does: its dictionary key IS the YAML key.
	key := NormalizeCatalogueKey(entry.Key)
	if key == "" {
		key = NormalizeCatalogueKey(display)
	}
	if key == "" {
		return PrebuiltServer{}, fmt.Errorf("mcpregistry: catalogue entry has no key")
	}
	if entry.TimeoutSeconds < 0 {
		return PrebuiltServer{}, fmt.Errorf("mcpregistry: timeout is negative")
	}
	if err := ValidatePrebuiltServer(entry); err != nil {
		return PrebuiltServer{}, err
	}

	headers := entry.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	encodedHeaders, err := json.Marshal(headers)
	if err != nil {
		return PrebuiltServer{}, fmt.Errorf("mcpregistry: encode headers: %w", err)
	}
	configSchema := entry.ConfigSchema
	if configSchema == nil {
		configSchema = map[string]any{"properties": map[string]any{}}
	}
	encodedConfigSchema, err := json.Marshal(configSchema)
	if err != nil {
		return PrebuiltServer{}, fmt.Errorf("mcpregistry: encode config schema: %w", err)
	}

	row := s.pool.QueryRow(ctx,
		`INSERT INTO elitea_mcp.prebuilt_servers
		     (catalogue_key, display_name, server_url, base_url, client_id,
		      client_secret_ref, timeout_seconds, headers, config_schema, enabled)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (catalogue_key) DO UPDATE SET
		     display_name      = EXCLUDED.display_name,
		     server_url        = EXCLUDED.server_url,
		     base_url          = EXCLUDED.base_url,
		     client_id         = EXCLUDED.client_id,
		     client_secret_ref = EXCLUDED.client_secret_ref,
		     timeout_seconds   = EXCLUDED.timeout_seconds,
		     headers           = EXCLUDED.headers,
		     config_schema     = EXCLUDED.config_schema,
		     enabled           = EXCLUDED.enabled,
		     updated_at        = now()
		 RETURNING `+prebuiltColumns,
		key, display, strings.TrimSpace(entry.ServerURL), strings.TrimSpace(entry.BaseURL),
		strings.TrimSpace(entry.ClientID), strings.TrimSpace(entry.ClientSecretRef),
		entry.TimeoutSeconds, encodedHeaders, encodedConfigSchema, entry.Enabled)

	return scanPrebuilt(row)
}

// Delete removes one catalogue entry and reports the secret reference it held.
//
// The reference is RETURNED rather than acted on, because deleting the vault
// entry is the admin path's business and it may not always be right: two
// catalogue entries could name the same platform credential. Returning it lets
// the caller decide, and makes the orphan visible either way.
func (s *PrebuiltStore) Delete(ctx context.Context, nameOrType string) (string, error) {
	if s == nil || s.pool == nil {
		return "", ErrNoPool
	}
	key := NormalizeCatalogueKey(nameOrType)
	if key == "" {
		return "", ErrPrebuiltNotFound
	}
	var reference string
	err := s.pool.QueryRow(ctx,
		`DELETE FROM elitea_mcp.prebuilt_servers
		  WHERE catalogue_key = $1
		 RETURNING client_secret_ref`, key).Scan(&reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPrebuiltNotFound
	}
	if err != nil {
		return "", err
	}
	return reference, nil
}

// scanRow is the shared shape of pgx.Row and pgx.Rows for a single record.
type scanRow interface {
	Scan(dest ...any) error
}

func scanPrebuilt(row scanRow) (PrebuiltServer, error) {
	var (
		entry          PrebuiltServer
		encodedHeaders []byte
		encodedSchema  []byte
	)
	if err := row.Scan(
		&entry.Key, &entry.DisplayName, &entry.ServerURL, &entry.BaseURL, &entry.ClientID,
		&entry.ClientSecretRef, &entry.TimeoutSeconds, &encodedHeaders, &encodedSchema, &entry.Enabled,
	); err != nil {
		return PrebuiltServer{}, err
	}
	// A header object that will not decode is reported rather than dropped. An
	// entry whose headers silently vanish authenticates nothing and fails at the
	// remote server with a message that names neither this row nor this service.
	if len(encodedHeaders) > 0 {
		if err := json.Unmarshal(encodedHeaders, &entry.Headers); err != nil {
			return PrebuiltServer{}, fmt.Errorf(
				"mcpregistry: catalogue entry %q has undecodable headers: %w", entry.Key, err)
		}
	}
	if len(encodedSchema) > 0 {
		decoder := json.NewDecoder(strings.NewReader(string(encodedSchema)))
		decoder.UseNumber()
		if err := decoder.Decode(&entry.ConfigSchema); err != nil {
			return PrebuiltServer{}, fmt.Errorf(
				"mcpregistry: catalogue entry %q has undecodable config schema: %w", entry.Key, err)
		}
	}
	if err := ValidatePrebuiltServer(entry); err != nil {
		return PrebuiltServer{}, err
	}
	return entry, nil
}
