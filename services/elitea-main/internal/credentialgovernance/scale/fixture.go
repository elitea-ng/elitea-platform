// Package scale is the synthetic scale fixture of the governed credential
// profile (ADR-0039, issue #1124 work item 4).
//
// Seed writes one policy revision and, for Teams projects of AppsPerTeam
// calling applications each, AgentsPerApp governed agents per calling
// application. Every agent gets an external agent identity, an active agent
// binding, a sign-in assertion and an Entra-style delegation, an active
// delegated_background mandate that names the delegation, and
// GrantsPerAgent toolkits, each with its own target binding, active
// static_secret credential reference, toolkit binding and active
// credential use grant.
//
// External identities, bindings, sign-ins, delegations, mandates and grants
// go through the production stores (repos and delegationstore), so each row
// is written with its security audit event, as in production. Target
// bindings, credential references and toolkit bindings have no production
// writer yet (migrate-references stages them); they are copied in bulk.
//
// The dimensions are synthetic (gate D7). Nothing measured over them says
// anything about a dimension it did not seed.
package scale

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Dimensions are the synthetic sizes. Zero fields take the defaults.
type Dimensions struct {
	Teams          int
	AppsPerTeam    int
	AgentsPerApp   int
	GrantsPerAgent int
	// Workers bounds the concurrent store calls of the seed.
	Workers int
}

// Default dimensions: 300 teams (projects) x 60 calling applications.
const (
	DefaultTeams          = 300
	DefaultAppsPerTeam    = 60
	DefaultAgentsPerApp   = 1
	DefaultGrantsPerAgent = 2
	defaultWorkers        = 16
	// maxGrantsPerAgent keeps one run's security context under its
	// credential reference bound.
	maxGrantsPerAgent = 16
)

// WithDefaults fills the zero fields.
func (d Dimensions) WithDefaults() Dimensions {
	if d.Teams == 0 {
		d.Teams = DefaultTeams
	}
	if d.AppsPerTeam == 0 {
		d.AppsPerTeam = DefaultAppsPerTeam
	}
	if d.AgentsPerApp == 0 {
		d.AgentsPerApp = DefaultAgentsPerApp
	}
	if d.GrantsPerAgent == 0 {
		d.GrantsPerAgent = DefaultGrantsPerAgent
	}
	if d.Workers == 0 {
		d.Workers = defaultWorkers
	}
	return d
}

// Agents is the number of governed agents the dimensions seed.
func (d Dimensions) Agents() int { return d.Teams * d.AppsPerTeam * d.AgentsPerApp }

func (d Dimensions) validate() error {
	switch {
	case d.Teams <= 0, d.AppsPerTeam <= 0, d.AgentsPerApp <= 0, d.Workers <= 0:
		return errors.New("scale: every dimension must be positive")
	case d.GrantsPerAgent <= 0 || d.GrantsPerAgent > maxGrantsPerAgent:
		return fmt.Errorf("scale: grants per agent must be 1..%d", maxGrantsPerAgent)
	}
	return nil
}

// Fixture constants. The operation is one the built-in Jira adapter
// declares, so the production operation catalogue serves it.
const (
	RevisionID       = "scale-r1"
	ProviderType     = "jira"
	Operation        = "jira.issue.read"
	IdentityProvider = "fake"
	ProposerID       = "1"
	ApproverID       = "2"
	DelegationAud    = "api://scale-middle"
	// firstUserID offsets the synthetic subject users.
	firstUserID = 100000
	// assertionLifetime stays under the custody store's 30-hour bound.
	assertionLifetime = 29 * time.Hour
	mandateLifetime   = 24 * time.Hour
)

// Policy is the fixture's policy revision document. Static secrets are not
// cached, so every redemption reads its secret store.
const Policy = `{"schema":"elitea.credential-policy.v1","allowed_identity_modes":["dedicated_agent"],` +
	`"vault_required_credential_classes":[],"max_mandate_ttl_seconds":86400,"audit_backlog_alert_threshold":100000}`

// Toolkit is one credentialed toolkit of an agent.
type Toolkit struct {
	ID                    int64
	TargetBindingID       string
	CredentialReferenceID string
	GrantID               string
	Generation            int64
	Settings              map[string]any
	Digest                [32]byte
}

// Agent is one seeded governed agent.
type Agent struct {
	Index                int
	TenantProjectID      int64
	CallingApplicationID string
	ApplicationID        int64
	ApplicationVersionID int64
	DefinitionDigest     [32]byte
	ExternalIdentityID   string
	BindingID            string
	SubjectUserID        int64
	DelegationID         string
	MandateID            string
	Toolkits             []Toolkit
}

// Fixture is what Seed wrote.
type Fixture struct {
	Dimensions Dimensions
	Revision   profile.PolicyRevision
	Agents     []Agent
	// Rows counts the rows written per table.
	Rows map[string]int
	// Elapsed is the wall time of the seed.
	Elapsed time.Duration
}

// MasterKey is the fixture's delegation custody key (test-only).
func MasterKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x5c
	}
	return key
}

// Seed writes the fixture into pool's database. The database must be
// migrated and must hold no fixture yet.
func Seed(ctx context.Context, pool *pgxpool.Pool, dims Dimensions) (*Fixture, error) {
	started := time.Now()
	dims = dims.WithDefaults()
	if err := dims.validate(); err != nil {
		return nil, err
	}
	audit, err := securityaudit.NewPostgresAppender("elitea-main/scale-fixture")
	if err != nil {
		return nil, err
	}
	governance, err := repos.NewCredentialGovernanceStore(pool)
	if err != nil {
		return nil, err
	}
	revision, err := putRevision(ctx, governance)
	if err != nil {
		return nil, err
	}
	custody, err := delegationstore.New(pool, MasterKey(), audit)
	if err != nil {
		return nil, err
	}
	f := &Fixture{Dimensions: dims, Revision: revision, Rows: map[string]int{}}
	f.Agents = plan(dims)

	if err := seedProjects(ctx, pool, dims.Teams); err != nil {
		return nil, err
	}
	if err := copyTargetsAndReferences(ctx, pool, f.Agents); err != nil {
		return nil, err
	}
	s := seeder{
		identities: repos.NewExternalAgentIdentities(pool, audit),
		bindings:   repos.NewAgentIdentityBindings(pool, audit),
		mandates:   repos.NewMandates(pool, audit),
		grants:     repos.NewCredentialGrants(pool, audit),
		custody:    custody,
		revision:   revision.ID,
		now:        time.Now().UTC(),
	}
	if err := parallel(ctx, dims.Workers, f.Agents, s.identity); err != nil {
		return nil, err
	}
	if err := copyToolkitBindings(ctx, pool, f.Agents); err != nil {
		return nil, err
	}
	if err := parallel(ctx, dims.Workers, f.Agents, s.authority); err != nil {
		return nil, err
	}
	if err := parallel(ctx, dims.Workers, f.Agents, s.grant); err != nil {
		return nil, err
	}
	if err := f.count(ctx, pool); err != nil {
		return nil, err
	}
	f.Elapsed = time.Since(started)
	return f, nil
}

func putRevision(ctx context.Context, store *repos.CredentialGovernanceStore) (profile.PolicyRevision, error) {
	doc, canonical, err := profile.ParsePolicyDocument([]byte(Policy))
	if err != nil {
		return profile.PolicyRevision{}, err
	}
	revision := profile.PolicyRevision{
		ID: RevisionID, Digest: profile.Digest(canonical), Document: doc, Canonical: canonical, CreatedBy: "scale-fixture",
	}
	if err := store.PutRevision(ctx, revision); err != nil {
		return profile.PolicyRevision{}, fmt.Errorf("scale: put the policy revision: %w", err)
	}
	return revision, nil
}

// plan names every row before anything is written.
func plan(d Dimensions) []Agent {
	agents := make([]Agent, 0, d.Agents())
	for team := range d.Teams {
		for app := range d.AppsPerTeam {
			for range d.AgentsPerApp {
				index := len(agents)
				a := Agent{
					Index:                index,
					TenantProjectID:      int64(team + 1),
					CallingApplicationID: fmt.Sprintf("scale-app-%d-%d", team+1, app+1),
					ApplicationID:        int64(index + 1),
					ApplicationVersionID: int64(index + 1),
					DefinitionDigest:     digestOf("definition", index),
					ExternalIdentityID:   newID(),
					BindingID:            newID(),
					SubjectUserID:        int64(firstUserID + index),
					MandateID:            newID(),
				}
				for g := range d.GrantsPerAgent {
					tk := Toolkit{
						ID:                    int64(index*d.GrantsPerAgent + g + 1),
						TargetBindingID:       newID(),
						CredentialReferenceID: newID(),
						GrantID:               newID(),
						Generation:            1,
					}
					tk.Settings = map[string]any{"jira_configuration": map[string]any{
						"elitea_title": fmt.Sprintf("jira-%d-%d", index, g), "private": false,
						"credential_reference_id": tk.CredentialReferenceID,
					}}
					digest, err := credentialgrant.ToolkitDigest(ProviderType, tk.Settings)
					if err != nil {
						// The settings are fixed above; a failure is a programming error.
						panic(err)
					}
					tk.Digest = digest
					a.Toolkits = append(a.Toolkits, tk)
				}
				agents = append(agents, a)
			}
		}
	}
	return agents
}

func newID() string { return uuid.Must(uuid.NewV7()).String() }

func digestOf(label string, index int) [32]byte {
	h := sha256.New()
	h.Write([]byte(label))
	h.Write(binary.BigEndian.AppendUint64(nil, uint64(index)))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// TargetIdentity is the canonical Jira site of a toolkit.
func TargetIdentity(agent, toolkit int) string {
	return fmt.Sprintf("https://site-%d-%d.jira.scale.test", agent, toolkit)
}

// Locator is the secret store path of a toolkit's static secret.
func Locator(agent, toolkit int) string {
	return fmt.Sprintf("scale/%d/%d", agent, toolkit)
}

// seedProjects makes the projects the execution kernel's foreign keys need.
func seedProjects(ctx context.Context, pool *pgxpool.Pool, teams int) error {
	_, err := pool.Exec(ctx, `
INSERT INTO centry.project (id, name, owner_id, create_success)
SELECT g, 'scale-team-' || g, 1, true FROM generate_series(1, $1::int) AS g
ON CONFLICT (id) DO NOTHING`, teams)
	if err != nil {
		return fmt.Errorf("scale: seed projects: %w", err)
	}
	return nil
}

func copyTargetsAndReferences(ctx context.Context, pool *pgxpool.Pool, agents []Agent) error {
	var targets, references [][]any
	for _, a := range agents {
		for g, tk := range a.Toolkits {
			targets = append(targets, []any{uuid.MustParse(tk.TargetBindingID), a.TenantProjectID, ProviderType, TargetIdentity(a.Index, g), ""})
			references = append(references, []any{
				uuid.MustParse(tk.CredentialReferenceID), a.TenantProjectID, string(credentialgrant.ClassStaticSecret),
				"pgvault", Locator(a.Index, g), ProviderType, uuid.MustParse(tk.TargetBindingID), tk.Generation,
				string(credentialgrant.ReferenceActive),
			})
		}
	}
	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"elitea_identity", "target_bindings"},
		[]string{"id", "tenant_project_id", "provider_type", "target_identity", "audience"}, pgx.CopyFromRows(targets)); err != nil {
		return fmt.Errorf("scale: copy target bindings: %w", err)
	}
	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"elitea_identity", "credential_references"},
		[]string{"id", "tenant_project_id", "class", "backend", "locator", "provider_type", "target_binding_id", "generation", "state"},
		pgx.CopyFromRows(references)); err != nil {
		return fmt.Errorf("scale: copy credential references: %w", err)
	}
	return nil
}

func copyToolkitBindings(ctx context.Context, pool *pgxpool.Pool, agents []Agent) error {
	var rows [][]any
	for _, a := range agents {
		for _, tk := range a.Toolkits {
			rows = append(rows, []any{uuid.MustParse(a.BindingID), tk.ID, a.TenantProjectID, tk.Digest[:]})
		}
	}
	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"elitea_identity", "agent_toolkit_bindings"},
		[]string{"agent_binding_id", "toolkit_id", "tenant_project_id", "toolkit_digest"}, pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("scale: copy toolkit bindings: %w", err)
	}
	return nil
}

// parallel runs step for every agent on at most workers goroutines. step
// may write its own agent's fields.
func parallel(ctx context.Context, workers int, agents []Agent, step func(context.Context, *Agent) error) error {
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(workers)
	for i := range agents {
		a := &agents[i]
		group.Go(func() error {
			if err := step(ctx, a); err != nil {
				return fmt.Errorf("scale: agent %d: %w", a.Index, err)
			}
			return nil
		})
	}
	return group.Wait()
}

type seeder struct {
	identities *repos.ExternalAgentIdentities
	bindings   *repos.AgentIdentityBindings
	mandates   *repos.Mandates
	grants     *repos.CredentialGrants
	custody    *delegationstore.Store
	revision   string
	now        time.Time
}

// identity writes the agent's external identity and active binding.
func (s seeder) identity(ctx context.Context, a *Agent) error {
	if _, err := s.identities.CreateExternalIdentity(ctx, agentidentity.ExternalAgentIdentity{
		ID: a.ExternalIdentityID, TenantProjectID: a.TenantProjectID, ApplicationID: a.ApplicationID,
		Mode: agentidentity.IdentityModeDedicatedAgent,
		Ref: agentidentity.ExternalRef{
			Provider: IdentityProvider, ProviderTenant: "scale", Kind: "fake.agent", ID: "obj-" + a.ExternalIdentityID,
		},
		State: agentidentity.ExternalIdentityActive, CreatedBy: ProposerID, LastTransitionBy: ApproverID,
		CreatedAt: s.now, UpdatedAt: s.now,
	}); err != nil {
		return fmt.Errorf("external identity: %w", err)
	}
	if _, err := s.bindings.CreateBinding(ctx, agentidentity.AgentIdentityBinding{
		ID: a.BindingID, TenantProjectID: a.TenantProjectID, ApplicationID: a.ApplicationID,
		ApplicationVersionID: a.ApplicationVersionID, DefinitionDigest: a.DefinitionDigest,
		Mode: agentidentity.IdentityModeDedicatedAgent, ExternalAgentIdentityID: a.ExternalIdentityID,
		State: agentidentity.BindingActive, PolicyRevisionID: s.revision,
		ProposedBy: ProposerID, ApprovedBy: ApproverID, LastTransitionBy: ApproverID, CreatedAt: s.now, UpdatedAt: s.now,
	}); err != nil {
		return fmt.Errorf("binding: %w", err)
	}
	return nil
}

// authority writes the subject's sign-in assertion, the delegation made
// from it and the delegated_background mandate that names the delegation.
func (s seeder) authority(ctx context.Context, a *Agent) error {
	session := "scale-session-" + strconv.Itoa(a.Index)
	if _, err := s.custody.RetainSignIn(ctx, delegationstore.SignIn{
		SessionID: session, UserID: a.SubjectUserID, Issuer: "https://login.scale.test/v2.0",
		TenantID: "0192f5e2-0000-7000-8000-00000000c0de", ObjectID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(session)).String(),
		Audience: DelegationAud, Scopes: []string{"Files.Read"}, Token: "scale-assertion-" + strconv.Itoa(a.Index),
		ExpiresAt: time.Now().Add(assertionLifetime),
	}); err != nil {
		return fmt.Errorf("sign-in assertion: %w", err)
	}
	delegation, err := s.custody.Delegate(ctx, delegationstore.DelegateRequest{
		SessionID: session, UserID: a.SubjectUserID, TenantProjectID: a.TenantProjectID, AgentBindingID: a.BindingID,
		Audience: DelegationAud, Scopes: []string{"Files.Read"},
	})
	if err != nil {
		return fmt.Errorf("delegation: %w", err)
	}
	a.DelegationID = delegation.ID
	subject, delegationID := a.SubjectUserID, delegation.ID
	if _, err := s.mandates.CreateMandate(ctx, mandate.Mandate{
		ID: a.MandateID, TenantProjectID: a.TenantProjectID, Kind: mandate.KindDelegatedBackground,
		AgentBindingID: a.BindingID, SubjectUserID: &subject, DelegationID: &delegationID,
		OperationScopes: []string{Operation}, NotBefore: s.now.Add(-time.Hour), ExpiresAt: s.now.Add(mandateLifetime),
		State: mandate.StateActive, PolicyRevisionID: s.revision, IssuedBy: ProposerID, ApprovedBy: ApproverID,
		LastTransitionBy: ApproverID, CreatedAt: s.now, UpdatedAt: s.now,
	}, func(agentidentity.AgentIdentityBinding, *mandate.Delegation) error { return nil }); err != nil {
		return fmt.Errorf("mandate: %w", err)
	}
	return nil
}

// grant writes one active use grant per toolkit.
func (s seeder) grant(ctx context.Context, a *Agent) error {
	for _, tk := range a.Toolkits {
		if _, err := s.grants.CreateGrant(ctx, credentialgrant.CredentialUseGrant{
			ID: tk.GrantID, TenantProjectID: a.TenantProjectID, AgentBindingID: a.BindingID, ToolkitID: tk.ID,
			CredentialReferenceID: tk.CredentialReferenceID, TargetBindingID: tk.TargetBindingID,
			Capabilities: []credentialgrant.Capability{credentialgrant.CapUse}, OperationScopes: []string{Operation},
			Obligations: []string{credentialgrant.ObligationAuditSync}, State: credentialgrant.GrantActive,
			ProposedBy: ProposerID, ApprovedBy: ApproverID, LastTransitionBy: ApproverID, CreatedAt: s.now, UpdatedAt: s.now,
		}); err != nil {
			return fmt.Errorf("grant: %w", err)
		}
	}
	return nil
}

// countedTables are the tables the report counts.
var countedTables = []string{
	"elitea_identity.credential_policy_revisions",
	"elitea_identity.external_agent_identities",
	"elitea_identity.agent_identity_bindings",
	"elitea_identity.signin_assertions",
	"elitea_identity.delegations",
	"elitea_identity.mandates",
	"elitea_identity.target_bindings",
	"elitea_identity.credential_references",
	"elitea_identity.agent_toolkit_bindings",
	"elitea_identity.credential_use_grants",
	"elitea_runtime.security_audit_outbox",
}

func (f *Fixture) count(ctx context.Context, pool *pgxpool.Pool) error {
	for _, table := range countedTables {
		var n int
		// The names are the constants above.
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			return fmt.Errorf("scale: count %s: %w", table, err)
		}
		f.Rows[table] = n
	}
	return nil
}

// CountedTables lists the tables of Fixture.Rows in report order.
func CountedTables() []string { return append([]string(nil), countedTables...) }
