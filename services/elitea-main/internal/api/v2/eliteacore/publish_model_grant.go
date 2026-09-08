package eliteacore

// The publish-time half of the platform-model GRANT.
//
// # The rule: a published agent needs a model granted to ALL projects
//
// Publishing already refuses a version whose model belongs to a project other
// than the catalogue's (`llm_not_shared`), because the catalogue is read by
// projects the author's own project has nothing to do with. The grant scope
// splits the catalogue's own models in two: a platform model may now be
// offered to every project, to none, or to a chosen few
// (application/configurations/model_grant.go).
//
// A published agent is offered to EVERYONE who opens the catalogue, so it is
// refused unless its model is granted to everyone as well. The alternative —
// publishing an agent whose model resolves for the projects on the list and
// answers `model_not_found` for the rest — moves the failure from the author,
// who can still change the model, to every reader of the catalogue, who
// cannot. It is the same argument the existing refusal makes, applied to the
// scope rather than to the owning project.
//
// The refusal keeps the `llm_not_shared` code, because it is the same finding
// and the publish dialog already renders it beside the model picker; the `msg`
// names the scope that was found, which is what says what to change.
//
// # Absence is NOT a refusal, and that is a decision
//
// A model name that matches no catalogue row leaves the publish alone. A
// version can name a model this lookup cannot reconstruct — a row seeded
// straight into the schema under a different title, a model addressed by a
// name the picker rewrote — and refusing on absence would turn "I could not
// find the row" into "you may not publish", which is the failure mode this
// platform has shipped more than once.
//
// That leniency costs nothing that matters, because publishing is not the
// enforcement. A project with no grant cannot SEE the model in its catalogue
// read and cannot dispatch it through the gateway; both of those refuse on the
// row itself. This check exists to tell an author at publish time rather than
// letting every reader discover it one message at a time.

import (
	"context"
	"fmt"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// publishModelGrantSQL reads the grant of the catalogue model a version names.
//
// The name is matched against BOTH the row's title and its `data.name`,
// because the two routes that serve the model catalogue disagree about which
// of them a version's `llm_settings.model_name` carries — the reviewed route
// answers `data.name` and the compatibility route answers the row title
// (e2e/fixtures/api.ts states the same split). Matching one alone would find
// the row on one deployment and miss it on the other, and a miss here is
// silent by the rule above.
//
// %s is the PUBLIC project's schema, already quoted by publicTenantSchema.
const publishModelGrantSQL = `
	SELECT c.data
	  FROM %s.configuration AS c
	 WHERE c.shared = true
	   AND c.type = ANY($1::text[])
	   AND (COALESCE(c.elitea_title, '') = $2 OR c.data->>'name' = $2)
	 ORDER BY c.id
	 LIMIT 1`

// publishModelTypes are the five configuration types that hold a model. The
// list mirrors the model sections the gateway dispatches; a type missing here
// makes a model of that kind unfindable, which reads as "no grant" and admits
// the publish.
var publishModelTypes = []string{
	"llm_model", "embedding_model", "image_generation_model", "asr_model", "tts_model",
}

// publishModelGrant answers the grant of the catalogue model named modelName,
// and whether a row was found at all.
//
// A query failure answers "not found", for the reason the file header gives:
// the enforcement is on the read and the dispatch, and a database blip must not
// become a refusal to publish.
func (h *Handler) publishModelGrant(
	ctx context.Context, modelName string,
) (configurationapp.ModelGrant, bool) {
	if h.pool == nil || modelName == "" {
		return configurationapp.ModelGrant{}, false
	}
	var data map[string]any
	err := h.pool.QueryRow(ctx,
		fmt.Sprintf(publishModelGrantSQL, publicTenantSchema()),
		publishModelTypes, modelName,
	).Scan(&data)
	if err != nil {
		return configurationapp.ModelGrant{}, false
	}
	return configurationapp.ReadModelGrant(data), true
}

// publishGrantRefusal reports the `msg` for a version whose catalogue model is
// not granted to every project, and "" when the version may be published.
//
// It reads `llm_settings` as the publish guard already decoded it, so the two
// rules cannot disagree about which model a version names.
func (h *Handler) publishGrantRefusal(ctx context.Context, llmSettings map[string]any) string {
	modelName, _ := llmSettings["model_name"].(string)
	grant, found := h.publishModelGrant(ctx, modelName)
	if !found || grant.Scope == configurationapp.ModelShareScopeAll {
		return ""
	}
	return fmt.Sprintf(
		"the platform model %q is available to %s, not to every project. A published agent is "+
			"offered to every project that reads the catalogue, so the model it names has to be "+
			"available to all projects. Change the model, or grant this one to all projects.",
		modelName, publishGrantScopeWords(grant),
	)
}

// publishGrantScopeWords says which scope was found, in words an operator can
// act on. The COUNT is given for a `projects` grant rather than the ids: the
// author of an agent is not the person who holds the admin screen, and a list
// of project ids they cannot open says less than the fact that the model is
// restricted at all.
func publishGrantScopeWords(grant configurationapp.ModelGrant) string {
	if grant.Scope == configurationapp.ModelShareScopeProjects {
		return fmt.Sprintf("%d selected project(s)", len(grant.Projects))
	}
	return "no project"
}
