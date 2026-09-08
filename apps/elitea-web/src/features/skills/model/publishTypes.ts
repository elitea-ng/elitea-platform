/**
 * The shapes the publishing surface reads, declared here rather than pulled
 * from `shared/api/generated/model` for the same reason `types.ts` next door
 * re-declares `SkillRecord`: the generated types are `zod.input<>` aliases with
 * mutable arrays and optional-everything, and a UI that has to null-check every
 * field of a response the server always sends ends up hiding real emptiness
 * behind the same guards.
 *
 * They are narrower than the wire, never wider — every field here exists in
 * `v2.yaml`'s `SkillValidationResult`, `PublicSkillListItem` and the
 * `attach_public_skill` response.
 */

/** One issue from the deterministic pre-publish gate. */
export interface SkillValidationIssue {
  readonly field?: string;
  readonly issue?: string;
  readonly fix?: string;
  readonly source?: string;
}

interface SkillValidationRecommendation {
  readonly field?: string;
  readonly suggestion?: string;
}

export interface SkillValidationReport {
  /** PASS, WARN or FAIL. */
  readonly status: string;
  readonly critical_issues: readonly SkillValidationIssue[];
  readonly warnings: readonly SkillValidationIssue[];
  readonly recommendations: readonly SkillValidationRecommendation[];
  readonly summary: string;
  /**
   * Always false in this stack. It is read rather than assumed so the dialog
   * can say "deterministic checks only" from the SERVER's answer — a client
   * that hardcoded it would keep saying it after the AI half arrives.
   */
  readonly ai_validation_available: boolean;
  /** Present on PASS/WARN; pins the content the gate approved. */
  readonly validation_token?: string;
}

export interface SkillCategory {
  readonly name: string;
  /** False for the categories an administrator added on the Features page. */
  readonly is_default?: boolean;
}

interface PublicSkillVersionSummary {
  readonly id?: number;
  readonly name?: string;
  readonly status?: string;
  readonly created_at?: string;
  readonly tags?: readonly string[];
}

export interface PublicSkillSummary {
  readonly id?: number;
  readonly name?: string;
  readonly description?: string;
  readonly owner_id?: number;
  readonly tags?: readonly string[];
  readonly versions?: readonly PublicSkillVersionSummary[];
  readonly has_published_version?: boolean;
}

export interface PublicSkillListPage {
  readonly rows: readonly PublicSkillSummary[];
  readonly total: number;
}

/**
 * One agent's outcome from an attach.
 *
 * The route answers 200 with a per-agent list even when some of them failed —
 * partial success is carried HERE and not in the HTTP status — so a caller that
 * only checks the status reports "attached" for an attach that attached
 * nothing.
 */
export interface AttachOutcome {
  readonly agent_version_id?: number;
  readonly ok?: boolean;
  readonly http_status?: number;
  readonly error?: string;
}

/**
 * One row of `GET /agents_with_skill/prompt_lib/{project}/{skill}` — an agent
 * version in this project that already has a fork of the public skill
 * attached. Matches `ListAgentsWithSkill200`'s row shape
 * (`shared/api/generated/model/listAgentsWithSkill200.zod.ts`).
 */
export interface AgentWithSkill {
  readonly application_id?: number;
  readonly name?: string;
  readonly entity_version_id?: number;
}
