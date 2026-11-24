package securityaudit

import (
	"fmt"
	"strconv"
)

// TypeAdminAccessChanged is a change of who holds what on the administration
// surface (ADR-0059 decision 8, "Security audit", story R1.5, issue 2509):
// one event per change, appended in the transaction of the change. Action is
// "admin.access.<kind>" for one of the AccessChangeKind values, and Access
// carries the before and after sets by name. The event names references and
// set names only: it carries the reason CODE and never the reason text, and a
// credential change records that the credential changed, never its value or
// a digest of it.
const TypeAdminAccessChanged EventType = "ai.elitea.security.v1.admin.access.changed"

// AccessChangeKind is the change kind of an admin.access.changed event.
type AccessChangeKind string

// The change kinds.
const (
	// AccessRoleAssignment: a person's roles in a central mode changed (a
	// role assigned or removed). Subject user; before and after are the
	// person's roles of the mode; Source is the grant history source of
	// R0.10 (console, api, scim, migration, bootstrap, operator, review).
	AccessRoleAssignment AccessChangeKind = "role_assignment"
	// AccessRolePermissions: a role's permission set was edited. Subject
	// role; before and after are its permissions.
	AccessRolePermissions AccessChangeKind = "role_permissions"
	// AccessRoleCreated and AccessRoleDeleted: a role was created or
	// deleted. Subject role; before (deleted) is the permission set it held,
	// and after is empty. A role still held is never deleted.
	AccessRoleCreated AccessChangeKind = "role_created"
	AccessRoleDeleted AccessChangeKind = "role_deleted"
	// AccessRoleRenamed: a role was renamed (issue 2994). Subject role, by
	// its NEW name, so it is the live role; before is the old name and after
	// the new one, so an earlier event of the old subject joins to it. A
	// central `default` role renamed with its project copies has Fields
	// ["project_role_copies"]. Grants and assignments follow the role id, so
	// no permission set changes.
	AccessRoleRenamed AccessChangeKind = "role_renamed"
	// AccessProjectPermissionsPushed: an estate-wide push wrote the
	// permissions of a project role in every project of a target set: the
	// personal-project or team-project matrix save, and Apply to Projects
	// (issue 2994). One summary event per role whose grants or assignments
	// the push changed, not one per project: a push reaches every project
	// at once. Subject project_roles, "<target>:<role>" (target personal or
	// team) on the platform partition. Before names the permissions the push
	// removed from the role in at least one project and after those it
	// added in at least one: disjoint sets, so after minus before is what
	// the push added. Fields is ["roles_assigned"] when the push also gave
	// the role to every member of the target projects. Mode is "default" for
	// Apply to Projects, which copies the central default matrix.
	AccessProjectPermissionsPushed AccessChangeKind = "project_permissions_pushed"
	// AccessTokenScopes: a personal access token was given administration
	// scopes. Subject token; before and after are its scopes. Wired by R1.3a
	// (issue 2507) through NewAdminAccessChanged.
	AccessTokenScopes AccessChangeKind = "token_scopes"
	// AccessProjectMembership: a project membership was written through a
	// central route. Subject user; the partition is the project; before and
	// after are the person's roles in the project; Membership carries the
	// facts R1.6 rates it by.
	AccessProjectMembership AccessChangeKind = "project_membership"
	// AccessGlobalProviderRoute: the endpoint, credential or keyless
	// identity of a global provider changed. Subject provider; Fields names
	// what changed.
	AccessGlobalProviderRoute AccessChangeKind = "global_provider_route"
	// AccessIdentityTrust: an action of identity.sso.trust.manage changed
	// identity trust. Subject the identity provider or the SCIM client;
	// Fields names what changed.
	AccessIdentityTrust AccessChangeKind = "identity_trust"
)

var accessChangeKinds = map[AccessChangeKind]bool{
	AccessRoleAssignment:           true,
	AccessRolePermissions:          true,
	AccessRoleCreated:              true,
	AccessRoleDeleted:              true,
	AccessRoleRenamed:              true,
	AccessProjectPermissionsPushed: true,
	AccessTokenScopes:              true,
	AccessProjectMembership:        true,
	AccessGlobalProviderRoute:      true,
	AccessIdentityTrust:            true,
}

// Valid reports whether k is a known change kind.
func (k AccessChangeKind) Valid() bool { return accessChangeKinds[k] }

// AccessChangeKinds lists every change kind, for a test that drives one per
// kind and for the docs.
func AccessChangeKinds() []AccessChangeKind {
	return []AccessChangeKind{
		AccessRoleAssignment, AccessRolePermissions, AccessRoleCreated, AccessRoleDeleted, AccessRoleRenamed,
		AccessProjectPermissionsPushed, AccessTokenScopes, AccessProjectMembership, AccessGlobalProviderRoute,
		AccessIdentityTrust,
	}
}

// Action answers the Action of an admin.access.changed event of kind k.
func (k AccessChangeKind) Action() string { return "admin.access." + string(k) }

// maxAccessSetItems bounds Before, After and Fields. A role can
// hold every registry permission, which is more than the 32 of a reason
// list, so the sets have a bound of their own.
const maxAccessSetItems = 1024

// AccessChange is the payload of an admin.access.changed event. Every value
// is a name, an id or a bounded enum: no free text. THERE IS NO REASON TEXT
// FIELD, and there must never be one (ADR-0059 decision 6: the text stays in
// the admin audit row; TestAccessChangeHasNoReasonText asserts it on the Go
// type and on the export schema).
type AccessChange struct {
	Kind AccessChangeKind
	// Before and After are the subject's set before and after the change, by
	// name, sorted. Empty for a kind that has no set (Fields says what
	// changed instead).
	Before []string
	After  []string
	// Fields names the fields a provider or trust change changed ("issuer",
	// "secret", "base_url", "deleted", "created"), never their values.
	Fields []string
	// Source is the grant history source of a role assignment (R0.10).
	Source string
	// Mode is the role mode of a role kind ("administration", "default").
	Mode string
	// ElevationID is the elevation the change was made under (R2.1), "" for
	// none.
	ElevationID string
	// ReasonCode is the reason code the caller gave (R3.8), "" for none.
	ReasonCode string
	// Membership is set on AccessProjectMembership only.
	Membership *MembershipFacts
}

// MembershipFacts are what R1.6 rates a central membership write by.
type MembershipFacts struct {
	// ActorIsMember: the actor is a member of the project.
	ActorIsMember bool
	// MemberHoldsAdminRole: the new member holds an administration role.
	MemberHoldsAdminRole bool
	// Bulk: the write is a bulk invite.
	Bulk bool
}

// accessData is the canonical JSON of AccessChange, inside eventData. Its
// field order and tags are part of the chain contract.
type accessData struct {
	Kind        string          `json:"kind"`
	Mode        string          `json:"mode,omitempty"`
	Source      string          `json:"source,omitempty"`
	Before      []string        `json:"before"`
	After       []string        `json:"after"`
	Fields      []string        `json:"fields,omitempty"`
	ElevationID string          `json:"elevation_id,omitempty"`
	ReasonCode  string          `json:"reason_code,omitempty"`
	Membership  *membershipData `json:"membership,omitempty"`
}

type membershipData struct {
	ActorIsMember        bool `json:"actor_is_member"`
	MemberHoldsAdminRole bool `json:"member_holds_admin_role"`
	Bulk                 bool `json:"bulk"`
}

func (c *AccessChange) data() *accessData {
	if c == nil {
		return nil
	}
	d := &accessData{
		Kind:        string(c.Kind),
		Mode:        c.Mode,
		Source:      c.Source,
		Before:      nonNil(c.Before),
		After:       nonNil(c.After),
		Fields:      c.Fields,
		ElevationID: c.ElevationID,
		ReasonCode:  c.ReasonCode,
	}
	if c.Membership != nil {
		d.Membership = &membershipData{
			ActorIsMember:        c.Membership.ActorIsMember,
			MemberHoldsAdminRole: c.Membership.MemberHoldsAdminRole,
			Bulk:                 c.Membership.Bulk,
		}
	}
	return d
}

// validate checks the payload against its event.
func (c *AccessChange) validate(e SecurityEvent) error {
	if e.Type != TypeAdminAccessChanged {
		if c != nil {
			return fmt.Errorf("%w: an access change belongs to %s only", ErrInvalidEvent, TypeAdminAccessChanged)
		}
		return nil
	}
	if c == nil {
		return fmt.Errorf("%w: %s requires an access change", ErrInvalidEvent, TypeAdminAccessChanged)
	}
	if !c.Kind.Valid() {
		return fmt.Errorf("%w: unknown access change kind", ErrInvalidEvent)
	}
	if e.Action != c.Kind.Action() {
		return fmt.Errorf("%w: the action of an access change is admin.access.<kind>", ErrInvalidEvent)
	}
	if (c.Membership != nil) != (c.Kind == AccessProjectMembership) {
		return fmt.Errorf("%w: membership facts belong to a project membership change only", ErrInvalidEvent)
	}
	if c.Source != "" && !reasonPattern.MatchString(c.Source) {
		return fmt.Errorf("%w: an access change source is not a code", ErrInvalidEvent)
	}
	if c.ReasonCode != "" && !reasonPattern.MatchString(c.ReasonCode) {
		return fmt.Errorf("%w: an access change reason_code is not a reason code", ErrInvalidEvent)
	}
	for name, value := range map[string]string{"mode": c.Mode, "elevation_id": c.ElevationID} {
		if err := checkString("access."+name, value); err != nil {
			return err
		}
	}
	for name, set := range map[string][]string{
		"before": c.Before, "after": c.After, "fields": c.Fields,
	} {
		if len(set) > maxAccessSetItems {
			return fmt.Errorf("%w: access.%s exceeds %d items", ErrInvalidEvent, name, maxAccessSetItems)
		}
		for _, item := range set {
			if item == "" {
				return fmt.Errorf("%w: access.%s has an empty item", ErrInvalidEvent, name)
			}
			if err := checkString("access."+name, item); err != nil {
				return err
			}
		}
	}
	return nil
}

// AdminAccessChanged is the input of NewAdminAccessChanged: who changed what.
type AdminAccessChanged struct {
	// ActorUserID is the person who made the change, 0 for none (the
	// first-login bootstrap, a migration).
	ActorUserID int64
	// SubjectType and SubjectID name what changed: "user"/<id>, "role"/
	// "<mode>:<name>", "project_roles"/"<target>:<name>", "token"/<id>, "global_provider"/<id>,
	// "identity_provider"/<key>, "scim_client"/<id>.
	SubjectType string
	SubjectID   string
	// TenantProjectID is the chain partition: 0 (the platform) for every
	// kind but a project membership, which is the project's.
	TenantProjectID int64
	// AuthenticationRef is the session or token row behind the change, never
	// the credential value.
	AuthenticationRef string
	// CorrelationID ties the event to the request (the trace id).
	CorrelationID string
	Change        AccessChange
}

// NewAdminAccessChanged builds the admin.access.changed event of one change.
// The appender assigns the id, the source and the time, and validates it.
func NewAdminAccessChanged(c AdminAccessChanged) SecurityEvent {
	change := c.Change
	principal := ""
	if c.ActorUserID > 0 {
		principal = "user:" + strconv.FormatInt(c.ActorUserID, 10)
	}
	return SecurityEvent{
		Type:              TypeAdminAccessChanged,
		TenantProjectID:   c.TenantProjectID,
		Action:            change.Kind.Action(),
		Effect:            EffectAllow,
		Reasons:           []string{"kind:" + string(change.Kind)},
		PrincipalRef:      principal,
		AuthenticationRef: c.AuthenticationRef,
		ResourceType:      c.SubjectType,
		ResourceID:        c.SubjectID,
		CorrelationID:     c.CorrelationID,
		Access:            &change,
	}
}
