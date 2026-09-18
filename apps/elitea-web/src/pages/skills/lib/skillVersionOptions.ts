/**
 * #917/ELITEA-3290,3291,3293,3294,3295,3296 — `SkillVersion[]` as the option
 * shape `features/agents`' `AgentPipelineVersionSelector` takes, plus the way
 * back from a picked option to the key the skill editor navigates by.
 *
 * WHY AN ADAPTER AND NOT A SHARED TYPE. The selector addresses a version by a
 * NUMERIC id (`AgentPipelineVersionOption.id`), because an application version
 * always has one. A skill version is addressed by `skillVersionKey` — its id
 * when the wire carries one, else its NAME, because a skill created
 * client-side has versions with no id until its first save. Rather than
 * widening the shared component's contract for that one case, every version
 * gets a stable numeric handle here: its own id when numeric, else a negative
 * synthetic derived from its position, which can never collide with a real
 * (always positive) `skill_versions.id`.
 *
 * Pure, so the mapping is unit-testable without mounting either component.
 */
import { skillVersionKey, type SkillVersion } from '@/features/skills';
import type { AgentPipelineVersionOption } from '@/features/agents';

/** The numeric handle one skill version is known by INSIDE the selector. */
export function versionOptionId(version: SkillVersion, index: number): number {
  const numeric = Number(version.id);
  return version.id !== undefined && Number.isFinite(numeric) ? numeric : -(index + 1);
}

export function toSkillVersionOptions(versions: readonly SkillVersion[]): readonly AgentPipelineVersionOption[] {
  return versions.map((version, index) => ({
    id: versionOptionId(version, index),
    name: version.name,
    ...(version.created_at === undefined ? {} : { created_at: version.created_at }),
    ...(version.status === undefined ? {} : { status: version.status }),
    ...(version.is_default === undefined ? {} : { is_default: version.is_default }),
    ...(version.author === undefined ? {} : { author: version.author }),
  }));
}

/** Option id -> the key `onNavigateVersion` expects, so a picked row routes to the right version. */
export function buildVersionKeyById(versions: readonly SkillVersion[]): ReadonlyMap<number, string> {
  return new Map(versions.map((version, index) => [versionOptionId(version, index), skillVersionKey(version)]));
}

/** The selector's `defaultVersionId` — the option id of the version the skill marks default, or `undefined` when none does. */
export function defaultVersionOptionId(versions: readonly SkillVersion[]): number | undefined {
  const index = versions.findIndex((version) => version.is_default === true);
  return index === -1 ? undefined : versionOptionId(versions[index]!, index);
}

/** The selector's `applicationVersionId` — the option id matching the key the editor currently has open. */
export function selectedVersionOptionId(versions: readonly SkillVersion[], selectedKey: string | undefined): number | undefined {
  const index = versions.findIndex((version) => skillVersionKey(version) === selectedKey);
  return index === -1 ? undefined : versionOptionId(versions[index]!, index);
}
