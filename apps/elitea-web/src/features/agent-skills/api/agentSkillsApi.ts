import { eliteaFetch } from '@/shared/api/generated/mutator';
import { listApplicationSkills } from '@/shared/api/generated/skills/skills';

/**
 * The agent↔skill link, as the Go service already serves it.
 *
 * Three routes, all of them live in `internal/api/router.go` today:
 *
 *  - `GET /elitea_core/application_skills/prompt_lib/{project}/{appVersionID}`
 *    — the skills attached to ONE agent version. Production calls exactly this
 *    URL from its own SKILLS section.
 *  - `GET /elitea_core/skills/prompt_lib/{project}` — the picker's list.
 *    Production calls it with
 *    `?sort_by=created_at&sort_order=desc&query=&limit=20&offset=0` when the
 *    "+ Skill" menu opens (measured read-only on next.elitea.ai 2026-09-06).
 *  - `PATCH /elitea_core/skill/prompt_lib/{project}/{skillID}` with a
 *    `has_relation` key — attach (201) or detach (200).
 *
 * The attach/detach route has no generated client: `has_relation` selects a
 * second operation inside the same handler
 * (`internal/api/v2/skills/handler.go` `updateSkillRelation`), which one
 * OpenAPI operation cannot express, so `v2.yaml` describes only the update. It
 * goes through `eliteaFetch` the way `features/skills` already reaches its own
 * un-spec'd routes, and carries its own hand-written manifest entry.
 */

interface Envelope<T> {
  readonly data: T;
}

function body<T>(envelope: unknown): T {
  return (envelope as Envelope<T>).data;
}

/** One skill, in the shape both read routes return. */
export interface AgentSkill {
  readonly id: number;
  readonly name: string;
  readonly description?: string;
  readonly version_id?: number;
  readonly version_name?: string;
  readonly versions?: readonly { readonly id: number; readonly name: string }[];
}

interface SkillListEnvelope {
  readonly items?: readonly AgentSkill[];
  readonly total?: number;
}

/**
 * The skills attached to one agent version.
 *
 * NOTE(#367): this route pointed at the project-wide skills List handler until
 * #367 and answered with EVERY skill in the project, at 200, in the same
 * envelope — so no caller could tell. It reads the attachment now.
 */
export async function fetchAttachedSkills(projectId: string, appVersionId: number): Promise<readonly AgentSkill[]> {
  const envelope = await listApplicationSkills(projectId, appVersionId);
  return body<SkillListEnvelope>(envelope).items ?? [];
}

/**
 * The picker's list — the project's skills, newest first, as production asks
 * for them.
 *
 * `sort_by`/`sort_order`/`query` are NOT in the generated `ListSkillsParams`:
 * `v2.yaml` documents only `page` and `page_size` for this operation, while
 * the handler reads all five (`internal/api/v2/skills/handler.go`). The URL is
 * therefore built here rather than through the generated params type, which
 * would silently drop the three keys that make the picker searchable. The spec
 * gap is real and belongs to whoever owns `v2.yaml`; narrowing the request to
 * match the spec would make the search box do nothing.
 */
export async function fetchProjectSkills(projectId: string, query: string): Promise<readonly AgentSkill[]> {
  const params = new URLSearchParams({
    sort_by: 'created_at',
    sort_order: 'desc',
    limit: '20',
    offset: '0',
  });
  if (query !== '') params.set('query', query);
  const envelope = await eliteaFetch<Envelope<SkillListEnvelope>>(
    `/elitea_core/skills/prompt_lib/${projectId}?${params.toString()}`,
    { method: 'GET' },
  );
  return envelope.data.items ?? [];
}

export interface SkillAttachment {
  readonly skill_id: number;
  readonly skill_version_id: number;
  readonly skill_name: string;
  readonly version_name: string;
}

/**
 * Attaches one skill to one agent version.
 *
 * `skill_version_id` is REQUIRED by the server, and it is not optional in
 * practice either: both readers of the row LEFT JOIN `skill_versions` through
 * it and serve `COALESCE(instructions, '')`, and the run-time registry then
 * DROPS a skill whose instructions are blank. An attachment with no skill
 * version is a row the agent cannot see.
 */
export async function attachSkill(
  projectId: string,
  skillId: number,
  entityVersionId: number,
  skillVersionId: number,
): Promise<SkillAttachment> {
  const envelope = await eliteaFetch<Envelope<SkillAttachment>>(
    `/elitea_core/skill/prompt_lib/${projectId}/${String(skillId)}`,
    {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        has_relation: true,
        entity_version_id: entityVersionId,
        entity_type: 'agent',
        skill_version_id: skillVersionId,
      }),
    },
  );
  return envelope.data;
}

export async function detachSkill(projectId: string, skillId: number, entityVersionId: number): Promise<void> {
  await eliteaFetch<Envelope<{ readonly ok: boolean }>>(
    `/elitea_core/skill/prompt_lib/${projectId}/${String(skillId)}`,
    {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      // `entity_type` rides along on the detach too: it is part of the row's
      // key `(entity_version_id, skill_id, entity_type)`, so omitting it
      // deletes nothing while answering 200.
      body: JSON.stringify({ has_relation: false, entity_version_id: entityVersionId, entity_type: 'agent' }),
    },
  );
}

/**
 * The cap the server enforces on the write side
 * (`MaxSkillsPerEntityVersion`, `internal/api/v2/skills/handler.go`) and the
 * number production renders as "n/5 skills added".
 */
export const MAX_SKILLS_PER_AGENT = 5;
