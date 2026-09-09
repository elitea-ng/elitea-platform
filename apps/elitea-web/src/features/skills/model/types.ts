import type { Skill as GeneratedSkill } from '@/shared/api/generated/model';

export interface SkillVersion {
  readonly id?: string | number;
  readonly name: string;
  readonly instructions: string;
  readonly tags: readonly string[];
  readonly meta?: Readonly<Record<string, unknown>>;
  /** "draft" or "published" (#249) — a published version is frozen server-side. */
  readonly status?: string;
  readonly created_at?: string;
  /** The version this one was cloned or restored from, if any (#874). */
  readonly parent_version_id?: string;
  /** Whether this is the version named by SkillRecord.default_version_id. */
  readonly is_default?: boolean;
}

/**
 * `versions` and `version_details` are omitted from the generated base before
 * being re-declared. Both were added to `Skill` in the spec and only appeared
 * here when the stale orval client was regenerated in Phase 1c, and both
 * conflict with this slice's local shapes rather than refining them:
 *
 *  - `versions` is generated MUTABLE; a `readonly` array is not assignable to
 *    a mutable one, so narrowing it in an `extends` clause is an error.
 *  - `version_details.id` is generated `string | undefined`, while
 *    `SkillVersion.id` is `string | number | undefined` — the wire carries
 *    numeric ids for some versions, so widening here is deliberate.
 *
 * Omitting first keeps the readonly-by-default convention without weakening
 * it. If the spec later tightens `version_details.id` to a single type, drop
 * it from the Omit and delete this note.
 */
export interface SkillRecord extends Omit<GeneratedSkill, 'versions' | 'version_details'> {
  readonly versions?: readonly SkillVersion[];
  readonly version_details?: SkillVersion;
}

export interface SkillDraft {
  readonly name: string;
  readonly description: string;
  readonly instructions: string;
  readonly tags: readonly string[];
}

export interface SkillListPage {
  readonly items: readonly SkillRecord[];
  readonly total: number;
  readonly page: number;
  readonly pageSize: number;
  readonly totalPages: number;
}

export interface SkillWriteInput {
  readonly name: string;
  readonly description: string;
  readonly instructions: string;
  readonly tags: readonly string[];
}

export interface SkillTestTurn {
  readonly role: 'user' | 'assistant';
  readonly content: string;
}

export interface SkillTestRequest {
  readonly sid: string;
  readonly messageId: string;
  readonly streamId: string;
  readonly instructions: string;
  readonly userInput: string;
  readonly chatHistory: readonly SkillTestTurn[];
  readonly modelName: string;
  readonly modelProjectId?: string;
  readonly temperature: number;
  readonly maxTokens: number;
}
