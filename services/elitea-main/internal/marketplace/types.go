// Package marketplace is the store behind the Marketplace (ADR-0051): the
// listings, the immutable listing-version snapshots a publish writes, the
// facet search, and the record of which project hired which version.
//
// It owns the elitea_marketplace schema (shared migration 0250). Callers
// pass a DB, which a pgx pool or a pgx transaction satisfies, so a publish
// writes its listing in the publish transaction.
//
// Rules that hold in every file:
//
//   - A requirement holds types and names, never a credential or a toolkit
//     setting. The snapshot of an agent keeps the tool and skill mappings as
//     requirements and the nested agents as embedded definitions.
//   - A schema name is an identifier produced by tenantschema, never a
//     caller value. Every other value is a bind parameter, and no search text
//     is formatted into SQL.
package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is what the store needs. *pgxpool.Pool and pgx.Tx satisfy it.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Kinds of listing.
const (
	KindAgent = "agent"
	KindSkill = "skill"
)

// Listing states.
const (
	ListingListed   = "listed"
	ListingDelisted = "delisted"
)

// Stored version states, and the state the API derives for the newest
// hireable version.
const (
	VersionAvailable  = "available"
	VersionDeprecated = "deprecated"
	VersionYanked     = "yanked"
	VersionCurrent    = "current"
)

// Hire modes.
const (
	ModeInstall = "install"
	ModeFork    = "fork"
)

// Origin kinds, as the agent's base version records them in meta.
const (
	OriginInstalled = "installed"
	OriginForked    = "forked"
)

// The meta keys an installed or forked agent carries (ADR-0051 decision 3).
const (
	MetaOriginKind        = "origin_kind"
	MetaOriginListingID   = "origin_listing_id"
	MetaOriginVersionID   = "origin_version_id"
	MetaOriginDigest      = "origin_digest"
	MetaOriginVersionName = "origin_version_name"
	MetaOriginListingName = "origin_listing_name"
)

// Tiers, lowest first. They match elitea_lifecycle.agent_registry.
var Tiers = []string{"low", "medium", "high", "critical"}

// TierRank is the position of tier in Tiers, or -1.
func TierRank(tier string) int {
	for i, t := range Tiers {
		if t == tier {
			return i
		}
	}
	return -1
}

// Requirement kinds.
const (
	RequirementToolkit = "toolkit"
	RequirementMCP     = "mcp"
	RequirementSkill   = "skill"
	RequirementAgent   = "agent"
)

// Requirement is one thing a listing version needs from the project. It
// holds the type and the names, never a credential.
type Requirement struct {
	Key           string   `json:"key"`
	Kind          string   `json:"kind"`
	Type          string   `json:"type"`
	Name          string   `json:"name"`
	Tools         []string `json:"tools"`
	HasWriteTools bool     `json:"has_write_tools"`
}

// Requirements are the requirements of one listing version.
type Requirements struct {
	Items         []Requirement `json:"items"`
	ModelClass    string        `json:"model_class"`
	Tier          string        `json:"tier"`
	HasWriteTools bool          `json:"has_write_tools"`
}

// Model classes.
const (
	ModelToolCalling = "tool_calling"
	ModelChat        = "chat"
)

// Types returns the toolkit and MCP types of the requirements, sorted and
// without duplicates: what the "requires" facet filters on.
func (r Requirements) Types() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, item := range r.Items {
		if item.Kind != RequirementToolkit && item.Kind != RequirementMCP {
			continue
		}
		if item.Type == "" || seen[item.Type] {
			continue
		}
		seen[item.Type] = true
		out = append(out, item.Type)
	}
	sort.Strings(out)
	return out
}

var writeVerbs = map[string]bool{
	"create": true, "update": true, "delete": true, "remove": true, "write": true, "edit": true,
	"push": true, "post": true, "put": true, "patch": true, "send": true, "merge": true,
	"commit": true, "add": true, "set": true, "upload": true, "execute": true, "run": true,
	"close": true, "comment": true, "assign": true, "approve": true, "reject": true,
	"publish": true, "deploy": true, "drop": true, "insert": true, "modify": true,
	"rename": true, "move": true, "transition": true, "trigger": true, "reply": true,
	"cancel": true, "archive": true, "revert": true, "reopen": true, "apply": true,
}

var wordSplit = regexp.MustCompile(`[^A-Za-z0-9]+|([a-z0-9])([A-Z])`)

// IsWriteTool reports whether a tool name reads as an action that changes
// something. The platform keeps no read-only flag on a tool, so the name is the
// only evidence: any word of the name that is a change verb makes it a write
// tool, and an unknown name reads as a read tool. The rule decides the
// approval of a hire, so it errs toward write: a name that carries a verb is a
// write tool even when it ends in a noun ("list_and_close_issues").
func IsWriteTool(name string) bool {
	spaced := wordSplit.ReplaceAllString(name, "$1 $2")
	for _, word := range strings.Fields(strings.ToLower(spaced)) {
		if writeVerbs[word] {
			return true
		}
	}
	return false
}

// DigestOf is the SHA-256 of a definition and its requirements: the digest an
// install pins. The definition is marshalled by encoding/json, which sorts map
// keys, so the same snapshot always has the same digest.
func DigestOf(definition json.RawMessage, requirements Requirements) ([32]byte, error) {
	var compact []byte
	var generic any
	if err := json.Unmarshal(definition, &generic); err != nil {
		return [32]byte{}, err
	}
	compact, err := json.Marshal(generic)
	if err != nil {
		return [32]byte{}, err
	}
	reqs, err := json.Marshal(requirements)
	if err != nil {
		return [32]byte{}, err
	}
	hash := sha256.New()
	hash.Write(compact)
	hash.Write([]byte{0})
	hash.Write(reqs)
	var out [32]byte
	copy(out[:], hash.Sum(nil))
	return out, nil
}

// Snapshot is what a publish records of a version.
type Snapshot struct {
	Kind         string
	Name         string
	Description  string
	Icon         *string
	Category     string
	Tags         []string
	SoulSummary  string
	Tier         string
	Definition   json.RawMessage
	Requirements Requirements
	Digest       [32]byte
}

// Errors the store answers.
var (
	ErrNotFound       = errors.New("marketplace: not found")
	ErrNotHireable    = errors.New("marketplace: the version is yanked, withdrawn or delisted")
	ErrInvalidState   = errors.New("marketplace: invalid state")
	ErrForbidden      = errors.New("marketplace: not allowed")
	ErrInvalidRequest = errors.New("marketplace: invalid request")
)

// Version is one listing version as the API reads it.
type Version struct {
	ID           int64
	Name         string
	State        string
	StateReason  string
	ReleaseNotes string
	Digest       string
	PublishedAt  time.Time
	Withdrawn    bool
	Requirements Requirements
}
