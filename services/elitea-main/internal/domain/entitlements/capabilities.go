// Package entitlements is the entitlement export of ADR-0044 decision 5
// (issue 1501): who holds which role on which project and which agent, and
// which governance capabilities each role carries.
//
// # Capabilities are derived, never assigned
//
// The platform authorises by PERMISSION string (auth_core__role_permission,
// auth_core__project_role_permission). A role has no capability of its own;
// it has the permissions its rows grant. So the export reads the grant
// tables and maps each permission to the capability labels below, in ONE
// table. A role→capability table would drift the first time an operator
// edited a role's matrix; a permission→capability table cannot, because the
// permission is what the router checks.
//
// # A new permission cannot escape the export
//
// internal/api/entitlements_capability_coverage_test.go walks every
// permission the router gates on and every permission a shared migration
// grants, and fails unless each one is either in Capabilities or in
// NotGovernanceRelevant. Adding a permission therefore forces a decision
// about whether an access review must see it.
package entitlements

import "sort"

// Capability is a governance capability label, stable across releases: an
// identity governance tool keys its certification items on it.
type Capability string

// The governance capabilities. The wire value is the string.
const (
	// ConfigureAgents creates, edits, versions, imports, forks and deletes
	// agents.
	ConfigureAgents Capability = "configure_agents"
	// ConfigureToolkits creates, edits and deletes toolkits.
	ConfigureToolkits Capability = "configure_toolkits"
	// ConfigureCredentials writes project configurations (where stored
	// credentials live) and governed credentials.
	ConfigureCredentials Capability = "configure_credentials"
	// ApproveCredentials approves a governed credential.
	ApproveCredentials Capability = "approve_credentials"
	// PublishAgents publishes and unpublishes agents and skills.
	PublishAgents Capability = "publish_agents"
	// ApprovePublication moderates what reaches the public catalogue.
	ApprovePublication Capability = "approve_publication"
	// ApproveChangeRequests approves agent change requests at every tier
	// (the governance role, ADR-0040).
	ApproveChangeRequests Capability = "approve_change_requests"
	// OverrideReleaseGates overrides a failed release gate (ADR-0040).
	OverrideReleaseGates Capability = "override_release_gates"
	// EditAgentRegistry edits an agent's registry record: owners, risk tier,
	// intended use. The governance role may edit any record and lower a
	// tier; an owner (granted_via owner) edits the record of the agent they
	// own and may raise its tier.
	EditAgentRegistry Capability = "edit_agent_registry"
	// Attest records and revokes attestations.
	Attest Capability = "attest"
	// MarkRecertificationDue sets a certified agent to due.
	MarkRecertificationDue Capability = "mark_recertification_due"
	// Suspend is the kill switch: suspend and resume execution.
	Suspend Capability = "suspend"
	// OverrideCaps changes spend caps and gateway governance limits, and lets
	// a run pass a run cap it reached (safety.override_cap).
	OverrideCaps Capability = "override_caps"
	// ApproveConnectors admits a connector (MCP server, service descriptor,
	// toolkit type) to the platform.
	ApproveConnectors Capability = "approve_connectors"
	// ManageRetention edits the retention policy.
	ManageRetention Capability = "manage_retention"
	// ManageLegalHold places and releases legal holds.
	ManageLegalHold Capability = "manage_legal_hold"
	// ReadRunEvidence reads run step records and their stored content.
	ReadRunEvidence Capability = "read_run_evidence"
	// ReadAuditTrail reads the admin audit trail.
	ReadAuditTrail Capability = "read_audit_trail"
	// ManageAccess grants roles, edits role permissions, creates, edits and
	// removes users and members, and sends invitations.
	ManageAccess Capability = "manage_access"
	// ManageSecrets writes, deletes and reveals secrets.
	ManageSecrets Capability = "manage_secrets"
	// ManageAgentIdentity manages agent identity bindings, mandates and
	// external agent identities (ADR-0039).
	ManageAgentIdentity Capability = "manage_agent_identity"
	// ApproveAgentIdentity approves agent identity bindings and mandates.
	ApproveAgentIdentity Capability = "approve_agent_identity"
	// ManageProjects creates, deletes and suspends projects.
	ManageProjects Capability = "manage_projects"
	// AdministerPlatform changes platform configuration: maintenance,
	// plugin configuration and native OAuth clients.
	AdministerPlatform Capability = "administer_platform"
	// ExportEntitlements downloads this export.
	ExportEntitlements Capability = "export_entitlements"
	// ReviewCredentials reads the credential inventory and the
	// cross-project permission trimming report.
	ReviewCredentials Capability = "review_credentials"
	// RevokeCredentials revokes an inventoried credential and removes an
	// unused grant across projects.
	RevokeCredentials Capability = "revoke_credentials"
	// TrimAgentPermissions removes an agent's unused tools, toolkits,
	// credential grants and egress hosts in a project.
	TrimAgentPermissions Capability = "trim_agent_permissions"
)

// ExportPermission gates the admin export (administration mode). Shared
// migration 0209 grants it to super_admin and admin.
const ExportPermission = "entitlements.export"

// ConnectorApprovePermission approves a connector (ADR-0044 decision 1). It
// is mapped by name; until the connector approval change lands nobody holds
// it, so it produces no row.
const ConnectorApprovePermission = "connector.approve"

// Capabilities maps each governance-relevant permission to the capabilities
// it carries. It is the ONLY place this mapping exists.
var Capabilities = map[string][]Capability{
	// Agents.
	"models.applications.applications.create":        {ConfigureAgents},
	"models.applications.application.update":         {ConfigureAgents},
	"models.applications.application.delete":         {ConfigureAgents},
	"models.applications.application_relation.patch": {ConfigureAgents},
	"models.applications.versions.create":            {ConfigureAgents},
	"models.applications.version.update":             {ConfigureAgents},
	"models.applications.version.delete":             {ConfigureAgents},
	"models.applications.export_import.import":       {ConfigureAgents},
	"models.applications.fork.post":                  {ConfigureAgents},
	// Skills are instructions an agent runs with.
	"models.applications.skills.create": {ConfigureAgents},
	"models.applications.skills.update": {ConfigureAgents},
	"models.applications.skills.delete": {ConfigureAgents},

	// Toolkits.
	"models.applications.tools.create": {ConfigureToolkits},
	"models.applications.tool.update":  {ConfigureToolkits},
	"models.applications.tool.patch":   {ConfigureToolkits},
	"models.applications.tool.delete":  {ConfigureToolkits},

	// Credentials.
	"configurations.configuration.create":   {ConfigureCredentials},
	"configurations.configuration.update":   {ConfigureCredentials},
	"configurations.configuration.delete":   {ConfigureCredentials},
	"models.credentials.governed.manage":    {ConfigureCredentials},
	"models.credentials.governed.approve":   {ApproveCredentials},
	"configuration.secrets.secret.create":   {ManageSecrets},
	"configuration.secrets.secret.edit":     {ManageSecrets},
	"configuration.secrets.secret.delete":   {ManageSecrets},
	"configuration.secrets.secret.unsecret": {ManageSecrets},

	// Publishing.
	"models.applications.publish.post":                 {PublishAgents},
	"models.applications.unpublish.post":               {PublishAgents},
	"models.applications.skills.publish":               {PublishAgents},
	"admin.moderation":                                 {ApprovePublication},
	"admin.moderation.edit":                            {ApprovePublication},
	"models.promptlib_shared.approve_collection.post":  {ApprovePublication},
	"models.promptlib_shared.reject_collection.delete": {ApprovePublication},

	// Agent lifecycle governance (ADR-0040, shared 0204).
	"models.applications.governance": {ApproveChangeRequests, OverrideReleaseGates, EditAgentRegistry},
	"lifecycle.attestations.write":   {Attest},
	"lifecycle.registry.mark_due":    {MarkRecertificationDue},

	// Kill switch (ADR-0041, shared 0194).
	"safety.suspend": {Suspend},
	// Run cap override (ADR-0041 decision 3, shared 0212): let one run
	// continue past a cap.
	"safety.override_cap": {OverrideCaps},

	// Caps and gates.
	"models.admin.project_budgets.edit": {OverrideCaps},
	"configuration.governance":          {OverrideCaps},

	// Connectors.
	ConnectorApprovePermission:         {ApproveConnectors},
	"provider_hub.descriptor.register": {ApproveConnectors},
	"provider_hub.descriptor.activate": {ApproveConnectors},
	"toolkit_catalogue.type.manage":    {ApproveConnectors},

	// Retention and legal hold (ADR-0042, shared 0195).
	"retention.manage":  {ManageRetention},
	"legal_hold.manage": {ManageLegalHold},

	// Evidence.
	"runs.evidence.read": {ReadRunEvidence},
	// Credential and access review (ADR-0044 decision 5, shared 0208).
	"credentials.inventory.read":    {ReviewCredentials},
	"credentials.inventory.manage":  {RevokeCredentials},
	"agents.trimming.manage":        {TrimAgentPermissions},
	"models.admin.audit_trail.view": {ReadAuditTrail},

	// Access.
	"admin.auth.users":                                  {ManageAccess},
	"admin.auth.users.super_admin":                      {ManageAccess},
	"modes.users":                                       {ManageAccess},
	"invites.bulkusers":                                 {ManageAccess},
	"invites.bulkprojects":                              {ManageAccess},
	"invites.platform":                                  {ManageAccess},
	"configuration.roles.roles.create":                  {ManageAccess},
	"configuration.roles.roles.edit":                    {ManageAccess},
	"configuration.roles.roles.delete":                  {ManageAccess},
	"configuration.roles.permissions.edit":              {ManageAccess},
	"configuration.roles.user_project_permissions.edit": {ManageAccess},
	"configuration.users.users.create":                  {ManageAccess},
	"configuration.users.users.edit":                    {ManageAccess},
	"configuration.users.users.delete":                  {ManageAccess},
	// Who may read and write an artifact bucket.
	"configuration.artifacts.s3_credentials.edit": {ManageAccess},

	// Agent identity (ADR-0039).
	"models.agent_identity.bindings.manage":  {ManageAgentIdentity},
	"models.agent_identity.mandates.manage":  {ManageAgentIdentity},
	"models.agent_identity.external.manage":  {ManageAgentIdentity},
	"models.agent_identity.bindings.approve": {ApproveAgentIdentity},
	"models.agent_identity.mandates.approve": {ApproveAgentIdentity},

	// Projects and the platform.
	"projects.projects.project.create": {ManageProjects},
	"projects.projects.project.delete": {ManageProjects},
	"projects.projects.projects.edit":  {ManageProjects},
	"runtime.plugins":                  {AdministerPlatform},
	"configuration.native_clients":     {AdministerPlatform},
	// A schedule runs a platform routine with no user principal.
	"configuration.scheduling.schedules.edit": {AdministerPlatform},

	ExportPermission: {ExportEntitlements},
}

// OwnerCapabilities are what being an agent's business or technical owner
// allows by itself (elitea_lifecycle.agent_registry): editing that agent's
// registry record (internal/infra/db/repos/agent_lifecycle.go UpdateRegistry).
var OwnerCapabilities = []Capability{EditAgentRegistry}

// NotGovernanceRelevant lists, by name, every permission an access review
// does not need: reads, chat, personal and content-authoring surfaces. A
// permission that is in neither this set nor Capabilities fails the
// coverage test, so it cannot silently escape the export.
var NotGovernanceRelevant = map[string]bool{
	"admin.moderation.create":                           true,
	"admin.moderation.view":                             true,
	"configuration.artifacts.artifacts.create":          true,
	"configuration.artifacts.artifacts.delete":          true,
	"configuration.artifacts.artifacts.edit":            true,
	"configuration.artifacts.artifacts.view":            true,
	"configuration.artifacts.s3_credentials.view":       true,
	"configuration.branding":                            true,
	"configuration.roles.permissions.view":              true,
	"configuration.roles.roles.view":                    true,
	"configuration.roles.user_project_permissions.view": true,
	"configuration.scheduling.schedules.view":           true,
	"configuration.secrets.secret.hide":                 true,
	"configuration.secrets.secret.list":                 true,
	"configuration.secrets.secret.view":                 true,
	"configuration.users.users.view":                    true,
	"configurations.configuration.details":              true,
	"configurations.configurations.list":                true,
	"models.admin.project_budgets.view":                 true,
	"models.applications.application.details":           true,
	"models.applications.applications.details":          true,
	"models.applications.applications.list":             true,
	"models.applications.deepwiki.generate":             true,
	"models.applications.deepwiki.read":                 true,
	"models.applications.evaluation.dataset.create":     true,
	"models.applications.evaluation.dataset.delete":     true,
	"models.applications.evaluation.dataset.read":       true,
	"models.applications.evaluation.dataset.update":     true,
	"models.applications.evaluation.dimension.create":   true,
	"models.applications.evaluation.dimension.delete":   true,
	"models.applications.evaluation.dimension.read":     true,
	"models.applications.evaluation.dimension.update":   true,
	"models.applications.evaluation.run.create":         true,
	"models.applications.evaluation.run.read":           true,
	"models.applications.export_import.export":          true,
	"models.applications.export_toolkit.export":         true,
	"models.applications.index_meta.delete":             true,
	"models.applications.index_meta.details":            true,
	"models.applications.index_meta.edit":               true,
	"models.applications.index_types.details":           true,
	"models.applications.inventory.invoke":              true,
	"models.applications.inventory.read":                true,
	"models.applications.predict.post":                  true,
	"models.applications.skills.details":                true,
	"models.applications.skills.export":                 true,
	"models.applications.skills.list":                   true,
	"models.applications.skills.upload_icon.delete":     true,
	"models.applications.skills.upload_icon.get":        true,
	"models.applications.skills.upload_icon.post":       true,
	"models.applications.skills.upload_icon.update":     true,
	"models.applications.task.delete":                   true,
	"models.applications.task.get":                      true,
	"models.applications.tool.details":                  true,
	"models.applications.tool.execute":                  true,
	"models.applications.toolkit_validator.check":       true,
	"models.applications.toolkits.details":              true,
	"models.applications.tools.list":                    true,
	"models.applications.trending_authors.list":         true,
	"models.applications.upload_icon.delete":            true,
	"models.applications.upload_icon.get":               true,
	"models.applications.upload_icon.post":              true,
	"models.applications.upload_icon.update":            true,
	"models.applications.version.details":               true,
	"models.applications.version_validator.check":       true,
	"models.applications.versions.get":                  true,
	"models.chat.attachments.create":                    true,
	"models.chat.attachments.delete":                    true,
	"models.chat.canvas.create":                         true,
	"models.chat.canvas.details":                        true,
	"models.chat.canvas.update":                         true,
	"models.chat.conversation.details":                  true,
	"models.chat.conversation.edit":                     true,
	"models.chat.conversation.update":                   true,
	"models.chat.conversations.create":                  true,
	"models.chat.conversations.delete":                  true,
	"models.chat.conversations.list":                    true,
	"models.chat.conversations.regenerate":              true,
	"models.chat.entity_settings.update":                true,
	"models.chat.folders.create":                        true,
	"models.chat.folders.delete":                        true,
	"models.chat.folders.get":                           true,
	"models.chat.folders.update":                        true,
	"models.chat.messages.create":                       true,
	"models.chat.messages.delete":                       true,
	"models.chat.messages.details":                      true,
	"models.chat.messages.list":                         true,
	"models.chat.participant.delete":                    true,
	"models.chat.participants.create":                   true,
	"models.chat.task.delete":                           true,
	"models.credentials.governed.probe":                 true,
	"models.credentials.governed.use":                   true,
	"models.monitoring.tracing.view":                    true,
	"models.notifications.notification.delete":          true,
	"models.notifications.notification.details":         true,
	"models.notifications.notification.update":          true,
	"models.notifications.notifications.list":           true,
	"models.project_context.edit":                       true,
	"models.project_context.view":                       true,
	"models.project_settings.edit":                      true,
	"models.promptlib_shared.search":                    true,
	"models.promptlib_shared.tags.list":                 true,
	"models.social.authors.get":                         true,
	"models.social.avatar.get":                          true,
	"models.social.avatar.update":                       true,
	"models.social.feedbacks.create":                    true,
	"models.social.feedbacks.list":                      true,
	"projects.projects.group.create":                    true,
	"projects.projects.group.delete":                    true,
	"projects.projects.groups.edit":                     true,
	"projects.projects.project.edit":                    true,
	"projects.projects.project.view":                    true,
	"projects.projects.projects.view":                   true,
	"runtime.admin.published_agents":                    true,
	"runtime.airun.serviceproviders":                    true,
}

// PermissionCapabilityPairs flattens Capabilities into two parallel slices,
// sorted, for a database query that joins grants to capabilities.
func PermissionCapabilityPairs() (permissions, capabilities []string) {
	names := make([]string, 0, len(Capabilities))
	for permission := range Capabilities {
		names = append(names, permission)
	}
	sort.Strings(names)
	for _, permission := range names {
		for _, capability := range Capabilities[permission] {
			permissions = append(permissions, permission)
			capabilities = append(capabilities, string(capability))
		}
	}
	return permissions, capabilities
}
