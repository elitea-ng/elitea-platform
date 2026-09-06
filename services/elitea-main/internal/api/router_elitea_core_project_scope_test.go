package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"

	v2analytics "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/analytics"
	v2convs "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	v2folders "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	v2tags "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/tags"
)

// ---------------------------------------------------------------------------
// #302 / #313 — /elitea_core project-scoped routes enforce a NAMED PERMISSION.
//
// Every handler in this group derives its tenant schema from the {projectID}
// path segment (tenantSchema(projectID), fmt.Sprintf("p_%s", projectID)) and
// none reads the caller's membership, so before #302 any authenticated
// principal could read and mutate any project by editing that segment.
//
// #302's first pass closed that at the MEMBERSHIP tier, which is a strictly
// weaker claim: it answers "is this caller in this project", never "may this
// caller do THIS". The three tests below are what separate the two, and the
// middle one — TestEliteaCoreRoutesRefuseAnUnderPrivilegedMember — is the only
// one that can. It is the test the issue asks for by name: without it,
// RequireProjectAccess and real RBAC are indistinguishable, because a member
// passes both and a non-member fails both.
// ---------------------------------------------------------------------------

// memberOfProject answers the membership EXISTS query for exactly one project
// and denies every other, which is what the real query does: it keys on
// (project_id, user_id) in auth_core__project_user_role, so a user with no
// role assignment in the named project resolves to no membership.
//
// It records the project ids it was asked about, so a test can prove the gate
// actually ran rather than inferring it from a status code that some other
// layer might also produce. Only the routes that still sit at the membership
// tier consult it — the tag writes, which have no legacy permission to copy,
// and the MCP surface, which pylon leaves unguarded.
type memberOfProject struct {
	project string
	asked   []string
}

func (m *memberOfProject) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	projectID := ""
	if len(args) > 0 {
		switch v := args[0].(type) {
		case int:
			projectID = strconv.Itoa(v)
		case string:
			projectID = v
		}
	}
	m.asked = append(m.asked, projectID)
	return membershipRow{allowed: projectID == m.project}
}

// membershipRow answers the middleware's two-column query: the first column is
// the membership decision, the second is "does this project exist?". Every
// project in this fixture exists, so the second column is always true and the
// only discriminator stays the membership answer.
type membershipRow struct{ allowed bool }

func (r membershipRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if target, ok := dest[0].(*bool); ok {
			*target = r.allowed
		}
	}
	if len(dest) > 1 {
		if target, ok := dest[1].(*bool); ok {
			*target = true
		}
	}
	return nil
}

// eliteaCoreProjectScopedRoute is one gated registration and the permission it
// resolves. The permission is the discriminator: it is what makes the
// under-privileged case constructible at all.
type eliteaCoreProjectScopedRoute struct {
	method string
	// ownPath and otherPath differ ONLY in the project segment. Anything else
	// differing would let a 403 come from routing rather than from the gate.
	ownPath   string
	otherPath string
	// permission is the string this route's gate names, transcribed from the
	// matching pylon module. router_elitea_core_permission_map_test.go proves
	// independently that every such string is one legacy declares and one the
	// migration corpus grants; this field is what lets a test WITHHOLD it.
	permission string
}

// One row per verb-bearing shape rather than one per registration, since every
// row in a family shares its middleware — but every distinct PERMISSION in the
// group is represented, because the permission is what is under test.
var eliteaCoreProjectScopedRoutes = []eliteaCoreProjectScopedRoute{
	// Agents and versions.
	{http.MethodGet, "/api/v2/elitea_core/applications/prompt_lib/7", "/api/v2/elitea_core/applications/prompt_lib/8", "models.applications.applications.list"},
	{http.MethodPost, "/api/v2/elitea_core/applications/prompt_lib/7", "/api/v2/elitea_core/applications/prompt_lib/8", "models.applications.applications.create"},
	{http.MethodGet, "/api/v2/elitea_core/application/prompt_lib/7/1", "/api/v2/elitea_core/application/prompt_lib/8/1", "models.applications.application.details"},
	{http.MethodPut, "/api/v2/elitea_core/application/prompt_lib/7/1", "/api/v2/elitea_core/application/prompt_lib/8/1", "models.applications.application.update"},
	{http.MethodDelete, "/api/v2/elitea_core/application/prompt_lib/7/1", "/api/v2/elitea_core/application/prompt_lib/8/1", "models.applications.application.delete"},
	{http.MethodGet, "/api/v2/elitea_core/versions/prompt_lib/7/1", "/api/v2/elitea_core/versions/prompt_lib/8/1", "models.applications.versions.get"},
	{http.MethodPost, "/api/v2/elitea_core/versions/prompt_lib/7/1", "/api/v2/elitea_core/versions/prompt_lib/8/1", "models.applications.versions.create"},
	{http.MethodGet, "/api/v2/elitea_core/version/prompt_lib/7/1/2", "/api/v2/elitea_core/version/prompt_lib/8/1/2", "models.applications.version.details"},
	{http.MethodPut, "/api/v2/elitea_core/version/prompt_lib/7/1/2", "/api/v2/elitea_core/version/prompt_lib/8/1/2", "models.applications.version.update"},
	{http.MethodDelete, "/api/v2/elitea_core/version/prompt_lib/7/1/2", "/api/v2/elitea_core/version/prompt_lib/8/1/2", "models.applications.version.delete"},
	// PATCH on the version path is the expanded READ the SDK calls, so it
	// carries the READ permission the GET above carries, not version.update
	// (#336). Pylon declares the same string on its own PATCH handler.
	{http.MethodPatch, "/api/v2/elitea_core/version/prompt_lib/7/1/2", "/api/v2/elitea_core/version/prompt_lib/8/1/2", "models.applications.version.details"},
	// Skills.
	{http.MethodGet, "/api/v2/elitea_core/skills/prompt_lib/7", "/api/v2/elitea_core/skills/prompt_lib/8", "models.applications.skills.list"},
	{http.MethodPost, "/api/v2/elitea_core/skills/prompt_lib/7", "/api/v2/elitea_core/skills/prompt_lib/8", "models.applications.skills.create"},
	{http.MethodGet, "/api/v2/elitea_core/skill/prompt_lib/7/1", "/api/v2/elitea_core/skill/prompt_lib/8/1", "models.applications.skills.details"},
	{http.MethodPut, "/api/v2/elitea_core/skill/prompt_lib/7/1", "/api/v2/elitea_core/skill/prompt_lib/8/1", "models.applications.skills.update"},
	// PATCH on the same path is overloaded: a body with `has_relation`
	// attaches or detaches a skill (#38). It writes to another project's
	// entity_skill_mapping, so it takes the same gate as the PUT beside it.
	// Pylon declares the same permission on its own PATCH handler
	// (legacy/plugins/elitea_core/api/v2/skill.py:202-207).
	{http.MethodPatch, "/api/v2/elitea_core/skill/prompt_lib/7/1", "/api/v2/elitea_core/skill/prompt_lib/8/1", "models.applications.skills.update"},
	{http.MethodDelete, "/api/v2/elitea_core/skill/prompt_lib/7/1", "/api/v2/elitea_core/skill/prompt_lib/8/1", "models.applications.skills.delete"},
	{http.MethodGet, "/api/v2/elitea_core/skill_export/prompt_lib/7/1", "/api/v2/elitea_core/skill_export/prompt_lib/8/1", "models.applications.skills.export"},
	// NOTE(#395): GET /application_skills/... stood here. #395 deleted the
	// prototype mount this table exercises; the reviewed route
	// (internal/api/v2/applicationskills) answers the path now and brings its
	// own cross-project and under-privileged cases in
	// TestCurrentApplicationSkillsHTTPPostgresRBACAndTenantMatrix.
	// Folders.
	{http.MethodGet, "/api/v2/elitea_core/folder/prompt_lib/7", "/api/v2/elitea_core/folder/prompt_lib/8", "models.chat.folders.get"},
	{http.MethodPost, "/api/v2/elitea_core/folder/prompt_lib/7", "/api/v2/elitea_core/folder/prompt_lib/8", "models.chat.folders.create"},
	{http.MethodPut, "/api/v2/elitea_core/folder/prompt_lib/7/1", "/api/v2/elitea_core/folder/prompt_lib/8/1", "models.chat.folders.update"},
	{http.MethodDelete, "/api/v2/elitea_core/folder/prompt_lib/7/1", "/api/v2/elitea_core/folder/prompt_lib/8/1", "models.chat.folders.delete"},
	// Tags — the READ only. The two writes have no legacy permission and are
	// covered by TestEliteaCoreTagWritesStayAtTheMembershipTier below.
	{http.MethodGet, "/api/v2/elitea_core/tags/prompt_lib/7", "/api/v2/elitea_core/tags/prompt_lib/8", "models.promptlib_shared.tags.list"},
	// Conversations, messages and the chat side surfaces.
	{http.MethodGet, "/api/v2/elitea_core/conversations/prompt_lib/7", "/api/v2/elitea_core/conversations/prompt_lib/8", "models.chat.conversations.list"},
	{http.MethodPost, "/api/v2/elitea_core/conversations/prompt_lib/7", "/api/v2/elitea_core/conversations/prompt_lib/8", "models.chat.conversations.create"},
	{http.MethodGet, "/api/v2/elitea_core/conversation/prompt_lib/7/1", "/api/v2/elitea_core/conversation/prompt_lib/8/1", "models.chat.conversation.details"},
	{http.MethodPut, "/api/v2/elitea_core/conversation/prompt_lib/7/1", "/api/v2/elitea_core/conversation/prompt_lib/8/1", "models.chat.conversation.update"},
	{http.MethodDelete, "/api/v2/elitea_core/conversation/prompt_lib/7/1", "/api/v2/elitea_core/conversation/prompt_lib/8/1", "models.chat.conversations.delete"},
	{http.MethodGet, "/api/v2/elitea_core/messages/prompt_lib/7/1", "/api/v2/elitea_core/messages/prompt_lib/8/1", "models.chat.messages.list"},
	{http.MethodDelete, "/api/v2/elitea_core/messages/prompt_lib/7/1", "/api/v2/elitea_core/messages/prompt_lib/8/1", "models.chat.messages.delete"},
	{http.MethodPost, "/api/v2/elitea_core/participants/prompt_lib/7/1", "/api/v2/elitea_core/participants/prompt_lib/8/1", "models.chat.participants.create"},
	{http.MethodDelete, "/api/v2/elitea_core/participant/prompt_lib/7/1/2", "/api/v2/elitea_core/participant/prompt_lib/8/1/2", "models.chat.participant.delete"},
	{http.MethodPut, "/api/v2/elitea_core/entity_settings/prompt_lib/7/1/2", "/api/v2/elitea_core/entity_settings/prompt_lib/8/1/2", "models.chat.entity_settings.update"},
	{http.MethodPost, "/api/v2/elitea_core/regenerate/prompt_lib/7/1", "/api/v2/elitea_core/regenerate/prompt_lib/8/1", "models.chat.conversations.regenerate"},
	{http.MethodPost, "/api/v2/elitea_core/canvases/prompt_lib/7", "/api/v2/elitea_core/canvases/prompt_lib/8", "models.chat.canvas.create"},
	{http.MethodGet, "/api/v2/elitea_core/canvas/prompt_lib/7/1", "/api/v2/elitea_core/canvas/prompt_lib/8/1", "models.chat.canvas.details"},
	{http.MethodPut, "/api/v2/elitea_core/canvas/prompt_lib/7/1", "/api/v2/elitea_core/canvas/prompt_lib/8/1", "models.chat.canvas.update"},
	{http.MethodPost, "/api/v2/elitea_core/attachments/prompt_lib/7/1", "/api/v2/elitea_core/attachments/prompt_lib/8/1", "models.chat.attachments.create"},
	{http.MethodDelete, "/api/v2/elitea_core/attachments/prompt_lib/7/1", "/api/v2/elitea_core/attachments/prompt_lib/8/1", "models.chat.attachments.delete"},
	{http.MethodGet, "/api/v2/elitea_core/context_strategy/prompt_lib/7/1", "/api/v2/elitea_core/context_strategy/prompt_lib/8/1", "models.chat.conversation.details"},
	{http.MethodPut, "/api/v2/elitea_core/context_strategy/prompt_lib/7/1", "/api/v2/elitea_core/context_strategy/prompt_lib/8/1", "models.chat.conversation.edit"},
	// Toolkits (the FEATURE_FLAG_TOOLKIT_PROJECT_ACCESS block, ON by default).
	{http.MethodGet, "/api/v2/elitea_core/tools/prompt_lib/7", "/api/v2/elitea_core/tools/prompt_lib/8", "models.applications.tools.list"},
	{http.MethodPost, "/api/v2/elitea_core/tools/prompt_lib/7", "/api/v2/elitea_core/tools/prompt_lib/8", "models.applications.tools.create"},
	{http.MethodGet, "/api/v2/elitea_core/tool/prompt_lib/7/1", "/api/v2/elitea_core/tool/prompt_lib/8/1", "models.applications.tool.details"},
	{http.MethodPut, "/api/v2/elitea_core/tool/prompt_lib/7/1", "/api/v2/elitea_core/tool/prompt_lib/8/1", "models.applications.tool.update"},
	{http.MethodPatch, "/api/v2/elitea_core/tool/prompt_lib/7/1", "/api/v2/elitea_core/tool/prompt_lib/8/1", "models.applications.tool.patch"},
	{http.MethodDelete, "/api/v2/elitea_core/tool/prompt_lib/7/1", "/api/v2/elitea_core/tool/prompt_lib/8/1", "models.applications.tool.delete"},
	{http.MethodGet, "/api/v2/elitea_core/toolkits/prompt_lib/7", "/api/v2/elitea_core/toolkits/prompt_lib/8", "models.applications.toolkits.details"},
	{http.MethodGet, "/api/v2/elitea_core/toolkit_validator/prompt_lib/7/1", "/api/v2/elitea_core/toolkit_validator/prompt_lib/8/1", "models.applications.toolkit_validator.check"},
	{http.MethodGet, "/api/v2/elitea_core/export_toolkit/prompt_lib/7/1", "/api/v2/elitea_core/export_toolkit/prompt_lib/8/1", "models.applications.export_toolkit.export"},
	// NOTE(#394): GET /index_types/... stood here, for the same reason and with
	// the same replacement: TestCurrentIndexTypesHTTPPostgresRBACAndTenantMatrix
	// (internal/api/v2/indextypes).
	{http.MethodGet, "/api/v2/elitea_core/index_meta/prompt_lib/7/1", "/api/v2/elitea_core/index_meta/prompt_lib/8/1", "models.applications.index_meta.details"},
	{http.MethodPatch, "/api/v2/elitea_core/index_meta/prompt_lib/7/1/2", "/api/v2/elitea_core/index_meta/prompt_lib/8/1/2", "models.applications.index_meta.edit"},
	{http.MethodDelete, "/api/v2/elitea_core/index_meta/prompt_lib/7/1/2", "/api/v2/elitea_core/index_meta/prompt_lib/8/1/2", "models.applications.index_meta.delete"},
	{http.MethodDelete, "/api/v2/elitea_core/index_cancel/prompt_lib/7/1/name/9", "/api/v2/elitea_core/index_cancel/prompt_lib/8/1/name/9", "models.applications.task.delete"},
	// Fork and the publish plane — the routes #302 names explicitly.
	{http.MethodPost, "/api/v2/elitea_core/fork/prompt_lib/7", "/api/v2/elitea_core/fork/prompt_lib/8", "models.applications.fork.post"},
	{http.MethodPost, "/api/v2/elitea_core/publish/prompt_lib/7/1", "/api/v2/elitea_core/publish/prompt_lib/8/1", "models.applications.publish.post"},
	{http.MethodPost, "/api/v2/elitea_core/unpublish/prompt_lib/7/1", "/api/v2/elitea_core/unpublish/prompt_lib/8/1", "models.applications.unpublish.post"},
	{http.MethodPost, "/api/v2/elitea_core/version_validator/prompt_lib/7/1/2", "/api/v2/elitea_core/version_validator/prompt_lib/8/1/2", "models.applications.version_validator.check"},
	{http.MethodPost, "/api/v2/elitea_core/publish_skill/prompt_lib/7/1/2", "/api/v2/elitea_core/publish_skill/prompt_lib/8/1/2", "models.applications.skills.publish"},
	// The families #313 lists as still ungated: icons, export/import, MCP
	// proxies, analytics, users/roles, project info/context, relations, search.
	{http.MethodGet, "/api/v2/elitea_core/upload_icon/prompt_lib/7", "/api/v2/elitea_core/upload_icon/prompt_lib/8", "models.applications.upload_icon.get"},
	{http.MethodPost, "/api/v2/elitea_core/upload_icon/prompt_lib/7", "/api/v2/elitea_core/upload_icon/prompt_lib/8", "models.applications.upload_icon.post"},
	{http.MethodPut, "/api/v2/elitea_core/upload_icon/prompt_lib/7/1", "/api/v2/elitea_core/upload_icon/prompt_lib/8/1", "models.applications.upload_icon.update"},
	{http.MethodDelete, "/api/v2/elitea_core/upload_icon/prompt_lib/7/icon", "/api/v2/elitea_core/upload_icon/prompt_lib/8/icon", "models.applications.upload_icon.delete"},
	{http.MethodGet, "/api/v2/elitea_core/upload_skill_icon/prompt_lib/7", "/api/v2/elitea_core/upload_skill_icon/prompt_lib/8", "models.applications.skills.upload_icon.get"},
	{http.MethodPost, "/api/v2/elitea_core/upload_skill_icon/prompt_lib/7", "/api/v2/elitea_core/upload_skill_icon/prompt_lib/8", "models.applications.skills.upload_icon.post"},
	{http.MethodPost, "/api/v2/elitea_core/upload_skill_icon/prompt_lib/7/1", "/api/v2/elitea_core/upload_skill_icon/prompt_lib/8/1", "models.applications.skills.upload_icon.post"},
	{http.MethodPut, "/api/v2/elitea_core/upload_skill_icon/prompt_lib/7/1", "/api/v2/elitea_core/upload_skill_icon/prompt_lib/8/1", "models.applications.skills.upload_icon.update"},
	{http.MethodDelete, "/api/v2/elitea_core/upload_skill_icon/prompt_lib/7/icon", "/api/v2/elitea_core/upload_skill_icon/prompt_lib/8/icon", "models.applications.skills.upload_icon.delete"},
	{http.MethodGet, "/api/v2/elitea_core/export_import/prompt_lib/7/1", "/api/v2/elitea_core/export_import/prompt_lib/8/1", "models.applications.export_import.export"},
	{http.MethodPost, "/api/v2/elitea_core/export_import/prompt_lib/7/1", "/api/v2/elitea_core/export_import/prompt_lib/8/1", "models.applications.export_import.import"},
	{http.MethodPost, "/api/v2/elitea_core/mcp_oauth_proxy/7", "/api/v2/elitea_core/mcp_oauth_proxy/8", "models.applications.tool.patch"},
	{http.MethodGet, "/api/v2/elitea_core/analytics/prompt_lib/7", "/api/v2/elitea_core/analytics/prompt_lib/8", v2analytics.ViewPermission},
	{http.MethodGet, "/api/v2/elitea_core/users/prompt_lib/7", "/api/v2/elitea_core/users/prompt_lib/8", "configuration.users.users.view"},
	{http.MethodGet, "/api/v2/elitea_core/roles/prompt_lib/7", "/api/v2/elitea_core/roles/prompt_lib/8", "configuration.roles.roles.view"},
	{http.MethodGet, "/api/v2/elitea_core/project_info/prompt_lib/7/project-info", "/api/v2/elitea_core/project_info/prompt_lib/8/project-info", "models.project_context.view"},
	{http.MethodPut, "/api/v2/elitea_core/project_info/prompt_lib/7/project-info", "/api/v2/elitea_core/project_info/prompt_lib/8/project-info", "models.project_context.edit"},
	{http.MethodPatch, "/api/v2/elitea_core/application_relation/prompt_lib/7/1/2", "/api/v2/elitea_core/application_relation/prompt_lib/8/1/2", "models.applications.application_relation.patch"},
	{http.MethodGet, "/api/v2/elitea_core/trending_authors/prompt_lib/7", "/api/v2/elitea_core/trending_authors/prompt_lib/8", "models.applications.trending_authors.list"},
	{http.MethodGet, "/api/v2/elitea_core/search_options/prompt_lib/7", "/api/v2/elitea_core/search_options/prompt_lib/8", "models.promptlib_shared.search"},
	// Batch version replacement and the two attachment-storage writes.
	{http.MethodPost, "/api/v2/elitea_core/batch_replace_version/prompt_lib/7/1/2", "/api/v2/elitea_core/batch_replace_version/prompt_lib/8/1/2", "models.applications.version.update"},

	// ── The three SIBLING groups #313 left behind ───────────────────────────
	//
	// These are not under /elitea_core. They are separate r.Route groups in the
	// same /api/v2 scope, and every path below names {projectID} and then reads
	// or writes that project. Each shipped with NO gate of any kind, so the
	// #302 hole this table was written for stayed open in three places the
	// table did not look. They are listed here rather than in a file of their
	// own so the three claims this table already makes — refuse another
	// project, refuse an under-privileged member, ADMIT an entitled one — apply
	// to them unchanged.

	// Notifications. The permissions are the four the pylon notifications
	// plugin declares, and the same four the reviewed copy of this surface
	// (internal/api/v2/notifications/api.go) gates the identical paths on.
	// 0079 grants them.
	{http.MethodGet, "/api/v2/notifications/notifications/prompt_lib/7", "/api/v2/notifications/notifications/prompt_lib/8", "models.notifications.notifications.list"},
	{http.MethodPut, "/api/v2/notifications/notifications/prompt_lib/7", "/api/v2/notifications/notifications/prompt_lib/8", "models.notifications.notification.update"},
	{http.MethodDelete, "/api/v2/notifications/notifications/prompt_lib/7", "/api/v2/notifications/notifications/prompt_lib/8", "models.notifications.notification.delete"},
	{http.MethodPut, "/api/v2/notifications/notification/prompt_lib/7/1", "/api/v2/notifications/notification/prompt_lib/8/1", "models.notifications.notification.update"},
	{http.MethodDelete, "/api/v2/notifications/notification/prompt_lib/7/1", "/api/v2/notifications/notification/prompt_lib/8/1", "models.notifications.notification.delete"},

	// Context manager. Pylon has NO context_manager module, so these two
	// permissions are not a transcription: they are the conversation strings
	// the router applies to every other Go-only route that acts on one
	// conversation. See the note at the registration for the reasoning, and
	// migrations/shared/0068 for the grant.
	{http.MethodGet, "/api/v2/context_manager/analytics/7/1", "/api/v2/context_manager/analytics/8/1", "models.chat.conversation.details"},
	{http.MethodGet, "/api/v2/context_manager/summaries/7/1", "/api/v2/context_manager/summaries/8/1", "models.chat.conversation.details"},
	{http.MethodPost, "/api/v2/context_manager/summaries/7/1", "/api/v2/context_manager/summaries/8/1", "models.chat.conversation.edit"},
	{http.MethodPost, "/api/v2/context_manager/optimize_context/7/1", "/api/v2/context_manager/optimize_context/8/1", "models.chat.conversation.edit"},
	{http.MethodPut, "/api/v2/context_manager/summary/7/1/2", "/api/v2/context_manager/summary/8/1/2", "models.chat.conversation.edit"},
	{http.MethodDelete, "/api/v2/context_manager/summary/7/1/2", "/api/v2/context_manager/summary/8/1/2", "models.chat.conversation.edit"},

	// The project member and role listings under /admin, in DEFAULT mode —
	// what the project settings page calls as /admin/{users,roles}/default/.
	// The /elitea_core fallback copies of these two rows are already in this
	// table above; these are the PRIMARY registrations, and they carried no
	// gate at all while the copies carried one. coreHandler.Users answers with
	// every member's email for whatever project id the path names.
	//
	// Their ADMINISTRATION-mode twins are static registrations resolved
	// centrally, so this project-scoped table cannot express them.
	// TestAdminProjectListingsAreGatedInBothModes below covers those.
	{http.MethodGet, "/api/v2/admin/users/default/7", "/api/v2/admin/users/default/8", "configuration.users.users.view"},
	{http.MethodGet, "/api/v2/admin/roles/default/7", "/api/v2/admin/roles/default/8", "configuration.roles.roles.view"},
}

// allEliteaCorePermissions is every permission named in the table above.
var allEliteaCorePermissions = func() []string {
	seen := map[string]struct{}{}
	permissions := make([]string, 0, len(eliteaCoreProjectScopedRoutes))
	for _, route := range eliteaCoreProjectScopedRoutes {
		if _, ok := seen[route.permission]; ok {
			continue
		}
		seen[route.permission] = struct{}{}
		permissions = append(permissions, route.permission)
	}
	return permissions
}()

// permissionsExcept is the whole point of the under-privileged case: a caller
// who holds everything this group grants EXCEPT one string.
func permissionsExcept(withheld string) []string {
	granted := make([]string, 0, len(allEliteaCorePermissions))
	for _, permission := range allEliteaCorePermissions {
		if permission != withheld {
			granted = append(granted, permission)
		}
	}
	return granted
}

// newEliteaCoreProjectScopeRouter composes every repo these families need, so
// all the routes are registered, and injects both authorization answers.
//
// The repositories are empty structs embedding the interface: they answer for
// ANY project id, exactly as alwaysSucceedsArtifactRepo does in
// router_artifacts_s3_test.go. That is deliberate and it is what makes the
// test meaningful — a repo that refused project 8 on its own would let the
// test pass without proving anything about authorization, so a refusal here
// can only have come from the gate in front of the handler. The resolver is
// the same shape: it answers for any project, and only the configured
// forProject/granted pair decides.
func newEliteaCoreProjectScopeRouter(
	querier *memberOfProject,
	resolver fakePermissionResolver,
) http.Handler {
	return NewRouter(RouterConfig{
		AuthValidator:             testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:        testPrincipalValidator{},
		AppsRepo:                  struct{ applications.Repository }{},
		SkillsRepo:                struct{ v2skills.Repository }{},
		FoldersRepo:               struct{ v2folders.Repository }{},
		TagsRepo:                  struct{ v2tags.Repository }{},
		ConvsRepo:                 struct{ v2convs.Repository }{},
		AnalyticsRepo:             struct{ v2analytics.Repository }{},
		ProjectAccessQuerier:      querier,
		ProjectPermissionResolver: resolver,
	})
}

// entitledForProjectSeven grants every permission in the table, scoped to
// project 7 alone — a fully-privileged member of one project and a stranger to
// every other.
func entitledForProjectSeven() fakePermissionResolver {
	return fakePermissionResolver{granted: allEliteaCorePermissions, forProject: "7"}
}

// TestEliteaCoreRoutesRefuseAnotherProject is #302's central claim. The
// principal is genuinely entitled to project 7, and holds every permission
// these routes name there. The only thing that changes between the two halves
// of each row is the project id in the path.
//
// Every row FAILS on the pre-#302 router: without the gate the request reaches
// the handler, which builds p_8 from the path segment and answers something
// other than 403.
func TestEliteaCoreRoutesRefuseAnotherProject(t *testing.T) {
	for _, route := range eliteaCoreProjectScopedRoutes {
		t.Run(route.method+" "+route.otherPath, func(t *testing.T) {
			router := newEliteaCoreProjectScopeRouter(
				&memberOfProject{project: "7"}, entitledForProjectSeven())

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(route.method, route.otherPath, nil)))

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 — the {projectID} path segment must not be "+
					"trusted as an authorization claim (#302); body=%s",
					recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestEliteaCoreRoutesRefuseAnUnderPrivilegedMember is the test #302's
// acceptance criteria name and the one #313 says is missing. It is the only
// case in this file that a membership check cannot also satisfy.
//
// The caller IS a member of project 7 — the resolver answers for that project,
// which is precisely what membership means to legacyrbac, since the central
// default-mode fallback is joined THROUGH the caller's assigned project roles
// and a non-member therefore resolves nothing. They hold every permission this
// group names except the ONE the route under test requires. A membership gate
// admits them; a permission gate must not.
//
// Every row passes trivially against RequireProjectAccess, which is why its
// absence left #302 half-closed.
func TestEliteaCoreRoutesRefuseAnUnderPrivilegedMember(t *testing.T) {
	for _, route := range eliteaCoreProjectScopedRoutes {
		t.Run(route.method+" "+route.ownPath+" without "+route.permission, func(t *testing.T) {
			router := newEliteaCoreProjectScopeRouter(
				&memberOfProject{project: "7"},
				fakePermissionResolver{granted: permissionsExcept(route.permission), forProject: "7"})

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(route.method, route.ownPath, nil)))

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 — a member of project 7 who does NOT hold %q reached "+
					"this route. Membership is not entitlement (#302, #313); body=%s",
					recorder.Code, route.permission, recorder.Body.String())
			}
		})
	}
}

// TestEliteaCoreRoutesAdmitAnEntitledMember is the control, and without it the
// two tests above would both pass against a router that refused EVERY request —
// which is the realistic failure mode of adding a gate whose backing grant is
// missing, and exactly what #313 says shipping the gating without the seeding
// migration would produce.
//
// It asserts "not 403" rather than a specific success code because the empty
// embedded repositories return zero values and each handler shapes its own
// response; what matters is that authorization let the request past.
func TestEliteaCoreRoutesAdmitAnEntitledMember(t *testing.T) {
	for _, route := range eliteaCoreProjectScopedRoutes {
		t.Run(route.method+" "+route.ownPath, func(t *testing.T) {
			router := newEliteaCoreProjectScopeRouter(
				&memberOfProject{project: "7"},
				fakePermissionResolver{granted: []string{route.permission}, forProject: "7"})

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(route.method, route.ownPath, nil)))

			if recorder.Code == http.StatusForbidden {
				t.Fatalf("status = 403 for a member holding exactly %q — the gate refuses the caller "+
					"its own permission entitles, which breaks the route rather than securing it; body=%s",
					route.permission, recorder.Body.String())
			}
			if recorder.Code == http.StatusNotFound || recorder.Code == http.StatusMethodNotAllowed {
				t.Fatalf("status = %d — the route is not registered, so the 403 assertions in the two "+
					"tests above would prove nothing", recorder.Code)
			}
		})
	}
}

// TestEliteaCoreTagWritesStayAtTheMembershipTier pins the one family #313
// leaves as a product decision. pylon's tags.py declares `get` and nothing
// else, so POST and DELETE have no permission to transcribe; they keep the
// membership check rather than being gated on an invented name (too strict:
// nothing grants it) or on the LIST permission (too loose: a viewer could then
// delete a project's tags).
//
// The assertion is on the membership query having RUN, not merely on the status
// code: a 403 from a permission gate would look identical from outside.
func TestEliteaCoreTagWritesStayAtTheMembershipTier(t *testing.T) {
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v2/elitea_core/tags/prompt_lib/8"},
		{http.MethodDelete, "/api/v2/elitea_core/tags/prompt_lib/8/1"},
	} {
		t.Run(route.method, func(t *testing.T) {
			querier := &memberOfProject{project: "7"}
			// Entitled everywhere, including project 8: only the membership
			// query can produce a refusal here.
			router := newEliteaCoreProjectScopeRouter(
				querier, fakePermissionResolver{granted: allEliteaCorePermissions})

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(route.method, route.path, nil)))

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 for a non-member; body=%s", recorder.Code, recorder.Body.String())
			}
			if len(querier.asked) != 1 || querier.asked[0] != "8" {
				t.Fatalf("membership was checked for %v, want exactly [8]: the tag writes are "+
					"supposed to sit at the membership tier, and this 403 came from somewhere else",
					querier.asked)
			}
		})
	}
}

// TestEliteaCoreProjectScopeRejectsMalformedProjectID proves the gate fails
// closed on input no project can ever match. The resolver parses the segment as
// a positive integer, so a value it accepts loosely — or one that reaches the
// handler regardless — would be a way past.
func TestEliteaCoreProjectScopeRejectsMalformedProjectID(t *testing.T) {
	for _, projectID := range []string{"0", "-1", "abc", "1.5", "7x"} {
		t.Run("projectID="+projectID, func(t *testing.T) {
			// Fully entitled to project 7 and to nothing else, which is the
			// only shape that makes this test discriminate: a malformed segment
			// is not project 7, so it must resolve nothing however the
			// middleware chooses to reject it.
			router := newEliteaCoreProjectScopeRouter(
				&memberOfProject{project: "7"}, entitledForProjectSeven())

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(
				http.MethodDelete, "/api/v2/elitea_core/application/prompt_lib/"+projectID+"/1", nil)))

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 for a malformed project id; body=%s",
					recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestAgentCategoriesStayUnscopedDespiteHavingAProjectInThePath is the case the
// public-route boundary test below could not see. It only covers routes with NO
// project in their path; agent_categories HAS one, so it looks scopable — and
// gating it emptied the Agent HUB's category rail, because the hub renders that
// list beside the ungated public catalogue and a non-member of the named project
// got a 403 where the catalogue beside it answered fine.
//
// The route serves a global taxonomy: nine hardcoded defaults plus a globally
// authored extras row. Its only project-shaped read is a `publishing_guardrail`
// lookup the handler itself documents as one that "could only ever miss",
// because no surface writes one. So there is nothing here to protect, and a
// caller who is neither a member nor entitled must still be served.
func TestAgentCategoriesStayUnscopedDespiteHavingAProjectInThePath(t *testing.T) {
	querier := &memberOfProject{project: "7"}
	// Entitled to nothing, anywhere. Any gate at all refuses this caller.
	router := newEliteaCoreProjectScopeRouter(querier, fakePermissionResolver{})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, testAuthHeader(
		httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/agent_categories/prompt_lib/8", nil)))

	if recorder.Code == http.StatusForbidden {
		t.Fatalf("a non-member got 403 from the global category taxonomy; body=%s", recorder.Body.String())
	}
	if len(querier.asked) != 0 {
		t.Fatalf("the membership query ran for a global taxonomy route: %v", querier.asked)
	}
}

// TestEliteaCoreLegacyUngatedRoutesStayUngated pins the boundary #302's
// acceptance criteria draw: "do not weaken any route that legacy leaves
// ungated". Every path here is one the machine-generated legacy catalogue lists
// among its thirty-seven UNGUARDED handlers, or one that names no project at
// all. If a future pass applied the gate group-wide instead of per-route, these
// would start answering 403 with nothing to resolve against.
func TestEliteaCoreLegacyUngatedRoutesStayUngated(t *testing.T) {
	querier := &memberOfProject{project: "7"}
	router := newEliteaCoreProjectScopeRouter(querier, fakePermissionResolver{})

	for _, path := range []string{
		// No project in the path at all.
		"/api/v2/elitea_core/public_applications/prompt_lib/",
		"/api/v2/elitea_core/public_skills/prompt_lib/",
		"/api/v2/elitea_core/author/prompt_lib/3",
		// A project in the path, and unguarded in pylon all the same.
		"/api/v2/elitea_core/default_icons/prompt_lib/8",
		"/api/v2/elitea_core/platform_settings/prompt_lib/8",
		// The caller's own permission self-read: gating it on the admin
		// plugin's matrix permission would 403 every editor and viewer on the
		// request the web app uses to decide what to render.
		"/api/v2/elitea_core/permissions/prompt_lib/8",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodGet, path, nil)))
			if recorder.Code == http.StatusForbidden {
				t.Fatalf("status = 403 on a route pylon leaves unguarded; body=%s",
					recorder.Body.String())
			}
		})
	}
	if len(querier.asked) != 0 {
		t.Fatalf("the membership query ran for an ungated route: %v", querier.asked)
	}
}
