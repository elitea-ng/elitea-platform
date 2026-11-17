package eliteacore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/entitylimits"
)

// The apply half of an upgrade and of an upstream merge (ADR-0051 decision 4).
// A request of kind upgrade or upstream_merge is applied in one transaction,
// either at once (a low-tier change is one click, recorded already approved) or
// by the final approval of its change request (repos.AgentLifecycle.
// finalizeMarketplace calls ApplyMarketplaceTx through its applier).

// ApplyMarketplaceTx applies an approved upgrade or upstream merge in tx.
func (h *Handler) ApplyMarketplaceTx(ctx context.Context, tx pgx.Tx, apply agentlifecycle.MarketplaceApply) error {
	schema, err := tenantschema.QuoteInt(apply.ProjectID)
	if err != nil {
		return fmt.Errorf("marketplace apply: project schema: %w", err)
	}
	var applied error
	origin := delegationgrant.OriginMarketplaceUpgrade
	switch apply.Kind {
	case agentlifecycle.KindUpgrade:
		applied = h.applyUpgradeTx(ctx, tx, schema, apply)
	case agentlifecycle.KindUpstreamMerge:
		applied = applyMergeTx(ctx, tx, schema, apply)
	default:
		return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "Only an upgrade or an upstream merge is applied here.")
	}
	if applied != nil {
		return applied
	}
	// The request approves the agent's internal edges: a widened edge is
	// re-approved by it (ADR-0044 amendment 1, A1.2 rule 3).
	proposer, approvers := apply.ProposerID, apply.ApproverIDs
	if apply.Kind == agentlifecycle.KindUpstreamMerge && len(approvers) == 0 {
		// A direct merge only moves the fork's origin; the fork's own edges are
		// its editors' and the drift check watches them. Only a merge a request
		// approved re-approves the edges.
		return nil
	}
	if len(approvers) == 0 && apply.ActorID > 0 {
		proposer, approvers = apply.ActorID, []int64{apply.ActorID}
	}
	return h.reconcileComposition(ctx, tx, repos.CompositionApproval{
		ProjectID: apply.ProjectID, RootApplicationID: apply.ApplicationID, Origin: origin,
		ProposerID: proposer, ApproverIDs: approvers,
	})
}

// loadTarget reads the target version of a change and checks that it is still
// the one the request named: present, with the same digest, and still
// hireable. A version yanked, withdrawn or delisted since the request is
// ErrNotHireable, which the approval answers as stale.
func loadTarget(ctx context.Context, tx pgx.Tx, apply agentlifecycle.MarketplaceApply) (marketplace.StoredVersion, error) {
	target, err := marketplace.LoadVersion(ctx, tx, apply.ListingID, apply.VersionID)
	if errors.Is(err, marketplace.ErrNotFound) {
		return marketplace.StoredVersion{}, agentlifecycle.Refuse(agentlifecycle.StaleCode, "The listing version no longer exists. Reject the request.")
	}
	if err != nil {
		return marketplace.StoredVersion{}, err
	}
	if target.Digest != apply.Digest {
		return marketplace.StoredVersion{}, agentlifecycle.Refuse(agentlifecycle.StaleCode, "The listing version changed after this request was made. Reject it.")
	}
	if !target.Hireable() {
		return marketplace.StoredVersion{}, marketplace.ErrNotHireable
	}
	return target, nil
}

func (h *Handler) applyUpgradeTx(ctx context.Context, tx pgx.Tx, schema string, apply agentlifecycle.MarketplaceApply) error {
	held, err := marketplace.LoadHeld(ctx, tx, apply.ProjectID, apply.ApplicationID, true)
	if errors.Is(err, marketplace.ErrNotFound) {
		return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The agent was not installed from the Marketplace.")
	}
	if err != nil {
		return err
	}
	if held.Mode != marketplace.ModeInstall {
		return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "A fork is updated through its upstream review, not upgraded.")
	}
	if !held.ToolsEnabled {
		return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The hire of this agent is not approved yet. Approve it first.")
	}
	if held.ListingID != apply.ListingID {
		return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The version belongs to another listing.")
	}
	target, err := loadTarget(ctx, tx, apply)
	if err != nil {
		return err
	}
	if held.VersionID == target.VersionID {
		return agentlifecycle.Refuse(agentlifecycle.StaleCode, "The agent is already on this version.")
	}
	prev, err := marketplace.LoadVersion(ctx, tx, held.ListingID, held.VersionID)
	if err != nil {
		return err
	}
	prevNode, err := marketplace.ParseDefinition(prev.Definition)
	if err != nil {
		return err
	}
	node, err := marketplace.ParseDefinition(target.Definition)
	if err != nil {
		return err
	}
	var payload marketplace.UpgradePayload
	if len(apply.Payload) > 0 {
		if err := json.Unmarshal(apply.Payload, &payload); err != nil {
			return fmt.Errorf("marketplace apply: read the upgrade payload: %w", err)
		}
	}

	// The bindings were checked when the request was made. An approver may act
	// days later, so they are checked again: a toolkit deleted or retyped since
	// then refuses the approval with a message instead of failing in the write.
	if err := revalidateBindings(ctx, tx, schema, target.Requirements, payload.Bindings); err != nil {
		return err
	}

	var baseVersion int64
	var metaText string
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id, meta::text FROM %s.application_versions
WHERE application_id = $1 AND meta ? 'origin_listing_id' ORDER BY id LIMIT 1`, schema), apply.ApplicationID).
		Scan(&baseVersion, &metaText); err != nil {
		return fmt.Errorf("marketplace apply: find the installed version: %w", err)
	}
	var meta map[string]any
	_ = json.Unmarshal([]byte(metaText), &meta) // the column holds an object
	if meta == nil {
		meta = map[string]any{}
	}
	var snapshotMeta map[string]any
	_ = json.Unmarshal(node.Version.Meta, &snapshotMeta) // the snapshot's meta is an object or null
	// A key the previous version set and this one no longer sets is dropped; a
	// key the project set itself stays.
	var previousMeta map[string]any
	_ = json.Unmarshal(prevNode.Version.Meta, &previousMeta) // the snapshot's meta is an object or null
	for key := range previousMeta {
		if _, kept := snapshotMeta[key]; !kept {
			delete(meta, key)
		}
	}
	for key, value := range snapshotMeta {
		meta[key] = value
	}
	if snapshotMeta == nil {
		snapshotMeta = map[string]any{}
	}
	var packDoc any
	_ = json.Unmarshal(node.Version.AvatarPack, &packDoc) // null reads as no pack
	avatar, avatarErr := h.carriedAvatarPack(ctx, map[string]any{"avatar_pack": packDoc, "meta": snapshotMeta}, strconv.FormatInt(apply.ProjectID, 10))
	if avatarErr != nil {
		// The files of the new avatar could not be copied: keep the one the agent has.
		slog.WarnContext(ctx, "marketplace apply: the avatar could not be copied", "application_id", apply.ApplicationID, "error", avatarErr)
	}
	meta[marketplace.MetaOriginKind] = marketplace.OriginInstalled
	meta[marketplace.MetaOriginListingID] = strconv.FormatInt(target.ListingID, 10)
	meta[marketplace.MetaOriginVersionID] = strconv.FormatInt(target.VersionID, 10)
	meta[marketplace.MetaOriginDigest] = hex.EncodeToString(target.Digest[:])
	meta[marketplace.MetaOriginVersionName] = target.Name
	meta[marketplace.MetaOriginListingName] = target.ListingName
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marketplace apply: encode the meta: %w", err)
	}

	// Take the old definition off: its tool mappings through the old bindings,
	// and its embedded agents.
	if err := marketplace.DetachTools(ctx, tx, schema, baseVersion, prevNode, marketplace.BindingMap(held.Bindings)); err != nil {
		return err
	}
	if err := removeNested(ctx, tx, schema, baseVersion, prevNode); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.application_versions SET llm_settings = $2::jsonb, instructions = $3, conversation_starters = $4::jsonb,
    welcome_message = $5, agent_type = $6, meta = $7::jsonb, pipeline_settings = $8::jsonb, soul = $9::jsonb,
    avatar_pack = CASE WHEN $10::boolean THEN $11::jsonb ELSE avatar_pack END
WHERE id = $1`, schema),
		baseVersion, jsonText(node.Version.LLMSettings, "{}"), node.Version.Instructions,
		jsonText(node.Version.ConversationStarters, "[]"), node.Version.WelcomeMessage, node.Version.AgentType,
		string(metaJSON), jsonText(node.Version.PipelineSettings, "{}"), jsonOrNil(node.Version.Soul),
		avatarErr == nil, avatar); err != nil {
		return fmt.Errorf("marketplace apply: write the definition: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.applications SET description = $2, updated_at = now() AT TIME ZONE 'UTC' WHERE id = $1`, schema),
		apply.ApplicationID, entitylimits.Truncate(node.Description, entitylimits.MaxDescriptionLength)); err != nil {
		return fmt.Errorf("marketplace apply: write the description: %w", err)
	}
	if err := writeTags(ctx, tx, schema, baseVersion, node.Tags); err != nil {
		return err
	}
	owner, err := tenantOwnerID(strconv.FormatInt(apply.ProjectID, 10))
	if err != nil {
		return err
	}
	author := apply.ActorID
	if author <= 0 {
		author = 1
	}
	if err := installNested(ctx, tx, schema, int(owner.Int64()), author, baseVersion, node); err != nil {
		return fmt.Errorf("marketplace apply: write the nested agents: %w", err)
	}
	if err := marketplace.AttachTools(ctx, tx, schema, apply.ApplicationID, baseVersion, node, marketplace.BindingMap(payload.Bindings)); err != nil {
		return err
	}
	if err := marketplace.MoveInstall(ctx, tx, apply.ProjectID, apply.ApplicationID, target, payload.Bindings); err != nil {
		return err
	}
	// The registry tier follows the listing up, never down: a person who set a
	// higher tier on the project keeps it.
	var current string
	if err := tx.QueryRow(ctx, `
SELECT risk_tier FROM elitea_lifecycle.agent_registry WHERE tenant_project_id = $1 AND application_id = $2`,
		apply.ProjectID, apply.ApplicationID).Scan(&current); err == nil {
		if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry SET risk_tier = $3, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`, apply.ProjectID, apply.ApplicationID,
			marketplace.MaxTier(current, target.Requirements.Tier)); err != nil {
			return fmt.Errorf("marketplace apply: set the tier: %w", err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("marketplace apply: read the tier: %w", err)
	}
	return nil
}

// revalidateBindings checks the bindings a request carries against the project
// as it is now: a toolkit or MCP requirement must be bound to a project toolkit
// of its type, a skill requirement to a project skill. A binding that fails
// refuses the approval as stale, with the reason in words.
func revalidateBindings(ctx context.Context, q relationQuerier, schema string, reqs marketplace.Requirements, bindings []marketplace.Binding) error {
	byKey := map[string]marketplace.Requirement{}
	for _, item := range reqs.Items {
		byKey[item.Key] = item
	}
	stale := func(msg string) error { return agentlifecycle.Refuse(agentlifecycle.StaleCode, msg) }
	for _, binding := range bindings {
		item, ok := byKey[binding.RequirementKey]
		if !ok || binding.Disabled || binding.ToolkitID <= 0 {
			continue
		}
		switch item.Kind {
		case marketplace.RequirementToolkit, marketplace.RequirementMCP:
			var found string
			err := q.QueryRow(ctx, fmt.Sprintf(`SELECT type FROM %s.elitea_tools WHERE id = $1`, schema), binding.ToolkitID).Scan(&found)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && found != item.Type) {
				return stale("The toolkit chosen for " + item.Type + " was deleted or changed since this request was made. Reject it and propose the upgrade again.")
			}
			if err != nil {
				return fmt.Errorf("marketplace apply: check a binding: %w", err)
			}
		case marketplace.RequirementSkill:
			var found string
			err := q.QueryRow(ctx, fmt.Sprintf(`SELECT name FROM %s.skills WHERE id = $1`, schema), binding.ToolkitID).Scan(&found)
			if errors.Is(err, pgx.ErrNoRows) {
				return stale("The skill chosen for " + item.Name + " was deleted since this request was made. Reject it and propose the upgrade again.")
			}
			if err != nil {
				return fmt.Errorf("marketplace apply: check a binding: %w", err)
			}
		}
	}
	return nil
}

// writeTags replaces the tags of a version.
func writeTags(ctx context.Context, q relationQuerier, schema string, versionID int64, tags []string) error {
	if _, err := q.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.application_version_tag_association WHERE version_id = $1`, schema), versionID); err != nil {
		return fmt.Errorf("marketplace apply: clear the tags: %w", err)
	}
	for _, tag := range tags {
		var tagID int64
		if err := q.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.tags (name, data) VALUES ($1, '{}'::jsonb)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name RETURNING id`, schema), tag).Scan(&tagID); err != nil {
			return fmt.Errorf("marketplace apply: write a tag: %w", err)
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.application_version_tag_association (version_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, schema),
			versionID, tagID); err != nil {
			return fmt.Errorf("marketplace apply: write a tag link: %w", err)
		}
	}
	return nil
}

// removeNested takes off the agents a snapshot's agent uses as tools: the
// reference from the parent version, then the embedded application and its
// versions, deepest first. An agent that is not found is skipped, since the
// goal is that none is left.
func removeNested(ctx context.Context, q relationQuerier, schema string, parentVersionID int64, node marketplace.DefinitionNode) error {
	for _, nested := range node.NestedAgents {
		var appText, versionText string
		err := q.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(t.settings->>'application_id', ''), COALESCE(t.settings->>'application_version_id', '')
FROM %[1]s.entity_tool_mapping m JOIN %[1]s.elitea_tools t ON t.id = m.tool_id
WHERE m.entity_version_id = $1 AND t.type = 'application' AND t.name = $2
ORDER BY m.id DESC LIMIT 1`, schema), parentVersionID, nested.ToolName).Scan(&appText, &versionText)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("marketplace apply: find the nested agent %q: %w", nested.ToolName, err)
		}
		childVersion, versionErr := strconv.ParseInt(versionText, 10, 64)
		childApp, appErr := strconv.ParseInt(appText, 10, 64)
		if versionErr == nil {
			if err := removeNested(ctx, q, schema, childVersion, nested.Node); err != nil {
				return err
			}
		}
		if _, err := deleteApplicationToolReferences(ctx, q, schema, strconv.FormatInt(parentVersionID, 10), appText, versionText); err != nil {
			return fmt.Errorf("marketplace apply: detach the nested agent %q: %w", nested.ToolName, err)
		}
		if appErr != nil {
			continue
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(
			`DELETE FROM %s.application_versions WHERE application_id = $1 AND status = 'embedded'`, schema), childApp); err != nil {
			return fmt.Errorf("marketplace apply: delete the nested versions: %w", err)
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`
DELETE FROM %[1]s.applications WHERE id = $1
  AND NOT EXISTS (SELECT 1 FROM %[1]s.application_versions WHERE application_id = $1)`, schema), childApp); err != nil {
			return fmt.Errorf("marketplace apply: delete the nested agent: %w", err)
		}
	}
	return nil
}

// applyMergeTx applies an upstream merge. The merge already wrote the draft
// (ProposeUpstreamMerge), so applying moves the fork's origin to the merged
// version: the next update diffs from there. It never changes the draft.
func applyMergeTx(ctx context.Context, tx pgx.Tx, schema string, apply agentlifecycle.MarketplaceApply) error {
	target, err := loadTarget(ctx, tx, apply)
	if err != nil {
		return err
	}
	var kind string
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(meta->>'origin_kind', 'forked') FROM %s.application_versions
WHERE application_id = $1 ORDER BY id LIMIT 1`, schema), apply.ApplicationID).Scan(&kind); err != nil {
		return fmt.Errorf("marketplace apply: read the fork: %w", err)
	}
	if kind == marketplace.OriginInstalled {
		return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "An installed agent is upgraded, not merged.")
	}
	return marketplace.MoveFork(ctx, tx, schema, apply.ProjectID, apply.ApplicationID, target, apply.ActorID)
}

// reconcileComposition approves the internal edges a Marketplace change wrote,
// in the change's transaction. Without a composed hook that can, it does
// nothing, and the edges keep whatever grants they have.
func (h *Handler) reconcileComposition(ctx context.Context, tx pgx.Tx, in repos.CompositionApproval) error {
	reconciler, ok := h.delegation.(compositionReconciler)
	if !ok {
		return nil
	}
	return reconciler.ReconcileCompositionTx(ctx, tx, in)
}

// compositionReconciler is the optional half of the delegation grant hook that
// approves the internal edges of an installed agent (repos.DelegationGrants).
type compositionReconciler interface {
	ReconcileCompositionTx(ctx context.Context, tx pgx.Tx, in repos.CompositionApproval) error
}
