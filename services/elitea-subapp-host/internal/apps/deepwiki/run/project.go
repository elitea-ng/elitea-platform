package run

// The project the engine's index is scoped to (engine migration 0005).
//
// WHY. The engine's index tables were keyed by wiki_id only, in one
// database for every project, and a wiki_id is derived from the repository,
// not the project. Each index row now belongs to one project, and every
// access is scoped by (project_id, wiki_id).
//
// The engine now keys every index row by (project_id, wiki_id) and refuses
// an index tool without the reserved argument ProjectArgument. THIS HOST is
// the only party that sets it, from authenticated context, and it overwrites
// whatever a caller put there.
//
// WHERE THE PROJECT COMES FROM, in order:
//
//  1. The verified identity of the hop (spi.Invoke.Identity.ProjectID). The
//     platform facade signs X-Elitea-Project-Id with the project in ITS URL
//     (/api/v2/deepwiki/tools/{project_id}/…), after it authorised the
//     caller for that project; spi.Server.identityGate strips every
//     identity header a caller presented and puts back only what a valid
//     HMAC signature vouches for.
//  2. With NO identity secret configured on this host (identity_verified
//     false in /health), there is no verified identity to read. The project
//     is then llm_settings.organization, which the facade OVERWRITES with
//     the same path project on every invoke (material.CallbackSettings; a
//     client's own llm_settings are discarded, tool-level ones lifted to
//     max_tokens/temperature only). That is only as trustworthy as the hop
//     to this host: mTLS keeps anything but the facade off it. A deployment
//     must configure the identity secret to make the project verified.
//  3. With a secret configured and no verified identity (an unsigned hop),
//     nothing is trusted and an index tool is refused.

import (
	"context"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// ProjectArgument is the engine's reserved argument for the authenticated
// project (services/elitea-deepwiki-engine, storage::PROJECT_ARG).
const ProjectArgument = "_elitea_project_id"

// IndexTools are the engine tools that read or write the index. Each is
// refused without an authenticated project.
var IndexTools = map[string]bool{
	"generate_wiki": true, "ask": true, "deep_research": true,
	// The index deletions (issue #1243): they delete rows, so they are
	// refused without an authenticated project like the rest.
	DeleteWikiIndexTool: true, DeleteProjectWikisTool: true,
}

// The engine tools that delete an index (services/elitea-deepwiki-engine,
// runner::maintenance).
const (
	DeleteWikiIndexTool    = "delete_wiki_index"
	DeleteProjectWikisTool = "delete_project_wikis"
)

type callerKey struct{}

// caller is what Runner.Invoke knows about WHO is calling, beyond the
// project: whether the hop's identity is signature-verified, and the user.
type caller struct {
	verified bool
	userID   string
}

func withCaller(ctx context.Context, c caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// RequirePlatformCaller refuses an invocation that does not come from the
// platform itself. A project-wide operation is called by elitea-main's
// project deprovisioning, which signs the project and NO user; a user
// session, whose hop always carries a user id, is refused. An unverified hop
// is refused too: without a signature the project is only as trustworthy as
// llm_settings, which is not enough to delete a whole project's index.
func RequirePlatformCaller(ctx context.Context, tool string) error {
	c, _ := ctx.Value(callerKey{}).(caller)
	if !c.verified {
		return spi.Failf(spi.KindValue, "%s needs a verified platform identity; this hop carries none", tool)
	}
	if c.userID != "" {
		return spi.Failf(spi.KindValue, "%s is a platform operation (project deletion) and cannot be run from a user session", tool)
	}
	return nil
}

// projectResolution is what Runner.Invoke learned about the caller's
// project: a validated id, or why there is none.
type projectResolution struct {
	id  string
	err error
}

type projectKey struct{}

func withProject(ctx context.Context, resolution projectResolution) context.Context {
	return context.WithValue(ctx, projectKey{}, resolution)
}

// ProjectFromContext is the authenticated project of the invocation ctx
// belongs to, or the reason there is none.
func ProjectFromContext(ctx context.Context) (string, error) {
	resolution, ok := ctx.Value(projectKey{}).(projectResolution)
	if !ok {
		return "", spi.Failf(spi.KindValue, "no authenticated project reached this tool; the wiki index is never read or written without one")
	}
	return resolution.id, resolution.err
}

// validProject accepts only a positive decimal id that fits the platform's
// INTEGER project id.
func validProject(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value <= 0 {
		return "", false
	}
	return strconv.FormatInt(value, 10), true
}

// TrustedProject resolves the project of one invocation from authenticated
// context — see the file comment for the order. verifiedHop reports whether
// this host verifies identity signatures (an identity secret is set).
func TrustedProject(identity spi.Identity, verifiedHop bool, params Params) (string, error) {
	if identity.ProjectID != "" {
		project, ok := validProject(identity.ProjectID)
		if !ok {
			return "", spi.Failf(spi.KindValue, "the verified identity names no valid project")
		}
		return project, nil
	}
	if verifiedHop {
		return "", spi.Failf(spi.KindValue, "this hop carried no verified identity, so no project can be trusted; the wiki index is never read or written without one")
	}
	organization := object(params["llm_settings"])["organization"]
	var raw string
	switch value := organization.(type) {
	case string:
		raw = value
	case float64:
		raw = strconv.FormatFloat(value, 'f', -1, 64)
	}
	if project, ok := validProject(raw); ok {
		return project, nil
	}
	return "", spi.Failf(spi.KindValue, "no authenticated project: the hop carried no identity and llm_settings names no project; the wiki index is never read or written without one")
}

// StampProject returns a copy of arguments with ProjectArgument set to the
// invocation's authenticated project, replacing any value a caller put
// there. An index tool without one is refused; any other tool has a stale
// value removed and goes on.
func StampProject(ctx context.Context, tool string, arguments map[string]any) (map[string]any, error) {
	stamped := make(map[string]any, len(arguments)+1)
	for key, value := range arguments {
		if key != ProjectArgument {
			stamped[key] = value
		}
	}
	project, err := ProjectFromContext(ctx)
	if err != nil || project == "" {
		if IndexTools[tool] {
			if err == nil {
				err = spi.Failf(spi.KindValue, "no authenticated project reached %s", tool)
			}
			return nil, err
		}
		return stamped, nil
	}
	stamped[ProjectArgument] = project
	return stamped, nil
}
