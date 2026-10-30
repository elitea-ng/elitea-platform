package entitlements

import (
	"encoding/csv"
	"io"
	"sort"
	"strconv"
	"strings"
)

// The scope of a grant.
const (
	ScopePlatform = "platform"
	ScopeProject  = "project"
	ScopeAgent    = "agent"
)

// How a grant is held.
const (
	// ViaRole: a platform role (auth_core__user_role) or a project role
	// (auth_core__project_user_role) carries the permission.
	ViaRole = "role"
	// ViaOwner: the user is an agent's business or technical owner in the
	// agent registry.
	ViaOwner = "owner"
	// ViaDirect is reserved for a per-user grant. The platform's resolver
	// (internal/infra/legacyrbac) reads no per-user permission table, so no
	// row carries it today; it is part of the documented vocabulary so an
	// importer need not change when one is added.
	ViaDirect = "direct"
)

// The role names of an agent registry owner.
const (
	RoleBusinessOwner  = "business_owner"
	RoleTechnicalOwner = "technical_owner"
)

// Grant is one export row: a user holds a role on a scope, and the role
// carries one capability through the listed permissions. A role that carries
// no governance capability is one row with an empty Capability, so the
// export still says who holds which role.
type Grant struct {
	UserID      int64
	UserName    string
	DisplayName string
	UserActive  bool

	ScopeType string
	// ScopeID is the administration mode ("administration", "developer") for
	// a platform role, the project id for a project role and the agent
	// (application) id for an agent ownership.
	ScopeID   string
	ScopeName string
	// ProjectID is the project of a project or agent scope, 0 for platform.
	ProjectID int64
	// ScopeActive is false for a suspended project and its agents: the
	// grant is dormant, not removed, and comes back on resume.
	ScopeActive bool

	Role string
	// RoleID is the auth_core__role id (platform) or auth_core__project_role
	// id (project); 0 for an owner. It names the SCIM group.
	RoleID int64

	Capability  string
	Permissions []string
	GrantedVia  string
}

// CSVHeader is the export's header row, in column order.
var CSVHeader = []string{
	"user_id", "user_name", "display_name", "user_active",
	"scope_type", "scope_id", "scope_name", "project_id", "scope_active",
	"role", "capability", "permissions", "granted_via",
}

// Record renders the grant as one CSV record in CSVHeader order.
//
// Permissions are space-separated (a permission name holds no space). A text
// cell a spreadsheet would read as a formula is prefixed with a quote.
func (g Grant) Record() []string {
	project := ""
	if g.ProjectID > 0 {
		project = strconv.FormatInt(g.ProjectID, 10)
	}
	return []string{
		strconv.FormatInt(g.UserID, 10), formulaSafe(g.UserName), formulaSafe(g.DisplayName),
		strconv.FormatBool(g.UserActive),
		g.ScopeType, formulaSafe(g.ScopeID), formulaSafe(g.ScopeName), project, strconv.FormatBool(g.ScopeActive),
		formulaSafe(g.Role), g.Capability, strings.Join(g.Permissions, " "), g.GrantedVia,
	}
}

// formulaSafe defuses a cell a spreadsheet would evaluate (CWE-1236).
func formulaSafe(value string) string {
	if value != "" && strings.ContainsRune("=+-@\t\r", rune(value[0])) {
		return "'" + value
	}
	return value
}

// CSVWriter streams grants as RFC 4180 CSV: a header row, CRLF line ends,
// and a field quoted when it holds a comma, a quote or a line break (with
// the quote doubled). UTF-8 is written as is, without a byte order mark.
type CSVWriter struct {
	out  *csv.Writer
	rows int
}

// NewCSVWriter writes the header row and returns the writer.
func NewCSVWriter(w io.Writer) (*CSVWriter, error) {
	out := csv.NewWriter(w)
	out.UseCRLF = true
	if err := out.Write(CSVHeader); err != nil {
		return nil, err
	}
	return &CSVWriter{out: out}, nil
}

// Write writes one grant. Rows are flushed every 256 grants so a large
// export reaches the client as it is produced.
func (c *CSVWriter) Write(g Grant) error {
	if err := c.out.Write(g.Record()); err != nil {
		return err
	}
	c.rows++
	if c.rows%256 == 0 {
		c.out.Flush()
		return c.out.Error()
	}
	return nil
}

// Close flushes what is buffered.
func (c *CSVWriter) Close() error {
	c.out.Flush()
	return c.out.Error()
}

// Rows is the number of grants written, not counting the header.
func (c *CSVWriter) Rows() int { return c.rows }

// UserEntitlements is one account and every grant it holds.
type UserEntitlements struct {
	UserID      int64
	UserName    string
	DisplayName string
	Active      bool
	Grants      []Grant
}

// Group is one role as a SCIM group: a platform role or a project role, its
// permissions and capabilities, and the accounts that hold it.
type Group struct {
	// ID is GroupID(ScopeType, RoleID).
	ID          string
	DisplayName string
	ScopeType   string
	ScopeID     string
	ScopeName   string
	ProjectID   int64
	ScopeActive bool
	Role        string
	RoleID      int64
	// Permissions are what the role grants: a project role's own rows when
	// the project saved a matrix for it, else the central default-mode role
	// of the same name (internal/infra/legacyrbac projectPermissions).
	Permissions  []string
	Capabilities []string
	Members      []Member
}

// Member is one account in a group.
type Member struct {
	UserID   int64
	UserName string
}

// The SCIM group id prefixes.
const (
	platformGroupPrefix = "platform-role-"
	projectGroupPrefix  = "project-role-"
)

// GroupID names the SCIM group of a role, or "" for a scope with no group
// (an agent ownership).
func GroupID(scopeType string, roleID int64) string {
	switch {
	case roleID <= 0:
		return ""
	case scopeType == ScopePlatform:
		return platformGroupPrefix + strconv.FormatInt(roleID, 10)
	case scopeType == ScopeProject:
		return projectGroupPrefix + strconv.FormatInt(roleID, 10)
	}
	return ""
}

// ParseGroupID reads a SCIM group id back into its scope type and role id.
func ParseGroupID(id string) (scopeType string, roleID int64, ok bool) {
	for prefix, scope := range map[string]string{platformGroupPrefix: ScopePlatform, projectGroupPrefix: ScopeProject} {
		if rest, found := strings.CutPrefix(id, prefix); found {
			value, err := strconv.ParseInt(rest, 10, 64)
			if err != nil || value <= 0 || strconv.FormatInt(value, 10) != rest {
				return "", 0, false
			}
			return scope, value, true
		}
	}
	return "", 0, false
}

// CapabilitiesOf maps permissions to their sorted, distinct capabilities.
func CapabilitiesOf(permissions []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, permission := range permissions {
		for _, capability := range Capabilities[permission] {
			if !seen[string(capability)] {
				seen[string(capability)] = true
				out = append(out, string(capability))
			}
		}
	}
	sort.Strings(out)
	return out
}
