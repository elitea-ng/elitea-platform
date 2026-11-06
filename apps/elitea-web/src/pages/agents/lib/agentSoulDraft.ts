import type { AgentSoulV1 } from '@/shared/api/generated/model';

/**
 * ADR-0050 decision 1a — the Persona tab's editable form of a version's soul
 * (`AgentSoulV1`). Every field is plain text so a textarea can hold it while
 * the user types: list fields are one entry per line, and relationships are
 * one `agent: stance` pair per line. `soulFromDraft` turns it back into the
 * wire object, and an empty draft becomes `null` — the version has no soul.
 */
export interface AgentSoulDraft {
  readonly displayName: string;
  readonly pronouns: string;
  readonly role: string;
  readonly voice: string;
  readonly values: string;
  readonly boundaries: string;
  readonly relationships: string;
  readonly proactiveGoals: string;
  readonly body: string;
}

export const SOUL_SCHEMA_ID = 'elitea.soul/v1';

/** The limit Main enforces: UTF-8 bytes of every string value, `schema` included. */
export const SOUL_MAX_TEXT_BYTES = 16 * 1024;

export const EMPTY_SOUL_DRAFT: AgentSoulDraft = {
  displayName: '',
  pronouns: '',
  role: '',
  voice: '',
  values: '',
  boundaries: '',
  relationships: '',
  proactiveGoals: '',
  body: '',
};

const DRAFT_KEYS = Object.keys(EMPTY_SOUL_DRAFT) as readonly (keyof AgentSoulDraft)[];

function joinLines(entries: readonly string[] | undefined): string {
  return (entries ?? []).join('\n');
}

function splitLines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '');
}

/** The stored soul -> the draft the tab edits. `null`/absent is the empty draft. */
export function soulToDraft(soul: AgentSoulV1 | null | undefined): AgentSoulDraft {
  if (soul === null || soul === undefined) return EMPTY_SOUL_DRAFT;
  return {
    displayName: soul.display_name ?? '',
    pronouns: soul.pronouns ?? '',
    role: soul.role ?? '',
    voice: joinLines(soul.voice),
    values: joinLines(soul.values),
    boundaries: joinLines(soul.boundaries),
    relationships: (soul.relationships ?? []).map((entry) => `${entry.agent}: ${entry.stance}`).join('\n'),
    proactiveGoals: joinLines(soul.proactive_goals),
    body: soul.body ?? '',
  };
}

/**
 * One `agent: stance` line. The FIRST colon splits, so a stance may hold
 * colons of its own; a line with no colon, or an empty half, is dropped
 * rather than sent for Main to refuse.
 */
function parseRelationship(line: string): { agent: string; stance: string } | undefined {
  const colon = line.indexOf(':');
  if (colon < 0) return undefined;
  const agent = line.slice(0, colon).trim();
  const stance = line.slice(colon + 1).trim();
  return agent === '' || stance === '' ? undefined : { agent, stance };
}

function withText<K extends string>(key: K, value: string): Partial<Record<K, string>> {
  const trimmed = value.trim();
  return trimmed === '' ? {} : ({ [key]: trimmed } as Record<K, string>);
}

function withList<K extends string>(key: K, value: string): Partial<Record<K, string[]>> {
  const lines = splitLines(value);
  return lines.length === 0 ? {} : ({ [key]: lines } as Record<K, string[]>);
}

/**
 * The draft -> the `soul` a save sends. Blank fields are left out (Main
 * refuses an empty string or an empty entry), and a draft with nothing in it
 * is `null`, which clears a stored soul.
 */
export function soulFromDraft(draft: AgentSoulDraft): AgentSoulV1 | null {
  const relationships = splitLines(draft.relationships)
    .map(parseRelationship)
    .filter((entry): entry is { agent: string; stance: string } => entry !== undefined);
  const body = draft.body.trim() === '' ? {} : { body: draft.body };
  const soul: AgentSoulV1 = {
    schema: SOUL_SCHEMA_ID,
    ...withText('display_name', draft.displayName),
    ...withText('pronouns', draft.pronouns),
    ...withText('role', draft.role),
    ...withList('voice', draft.voice),
    ...withList('values', draft.values),
    ...withList('boundaries', draft.boundaries),
    ...(relationships.length === 0 ? {} : { relationships }),
    ...withList('proactive_goals', draft.proactiveGoals),
    ...body,
  };
  return Object.keys(soul).length === 1 ? null : soul;
}

export function areSoulDraftsEqual(a: AgentSoulDraft, b: AgentSoulDraft): boolean {
  return DRAFT_KEYS.every((key) => a[key] === b[key]);
}

/** Main's size rule, mirrored: the UTF-8 bytes of every string value the save would send. */
export function soulTextBytes(soul: AgentSoulV1 | null): number {
  if (soul === null) return 0;
  const encoder = new TextEncoder();
  const count = (value: unknown): number => {
    if (typeof value === 'string') return encoder.encode(value).length;
    if (Array.isArray(value)) return value.reduce<number>((sum, entry) => sum + count(entry), 0);
    if (value !== null && typeof value === 'object') {
      return Object.values(value).reduce<number>((sum, entry) => sum + count(entry), 0);
    }
    return 0;
  };
  return count(soul);
}

/**
 * The `soul` key a version write sends. An edited draft is sent as the object
 * it describes (or `null` when emptied, which clears the stored soul). An
 * UNEDITED draft, or none at all, forwards the stored soul verbatim instead of
 * re-reading it through the draft: the draft trims and drops blank lines, and
 * a Save that touched nothing else must not rewrite the soul, since that would
 * move the release digest for an edit nobody made. With no stored soul the key
 * is left out, which the PUT reads as "leave alone" and the POST as "none".
 */
export function soulWriteField(
  stored: AgentSoulV1 | null | undefined,
  draft: AgentSoulDraft | undefined,
): { soul?: AgentSoulV1 | null } {
  if (draft !== undefined && !areSoulDraftsEqual(draft, soulToDraft(stored))) {
    return { soul: soulFromDraft(draft) };
  }
  return stored === null || stored === undefined ? {} : { soul: stored };
}

/** Per-entry and entry-count bounds of `AgentSoulV1`, for the list fields. */
const LIST_BOUNDS = {
  voice: { count: 16, each: 64 },
  values: { count: 32, each: 256 },
  boundaries: { count: 32, each: 512 },
  proactiveGoals: { count: 16, each: 512 },
} as const;

const RELATIONSHIP_BOUNDS = { count: 32, agent: 128, stance: 512 } as const;

function listBroken(text: string, bounds: { count: number; each: number }): boolean {
  const lines = splitLines(text);
  return lines.length > bounds.count || lines.some((line) => line.length > bounds.each);
}

function relationshipsBroken(text: string): boolean {
  const lines = splitLines(text);
  if (lines.length > RELATIONSHIP_BOUNDS.count) return true;
  return lines.some((line) => {
    const entry = parseRelationship(line);
    // A line Main would never see (no colon, an empty half) is dropped on
    // save, which would lose what the person typed: flag it instead.
    return entry === undefined || entry.agent.length > RELATIONSHIP_BOUNDS.agent || entry.stance.length > RELATIONSHIP_BOUNDS.stance;
  });
}

/**
 * The Persona fields a save would be refused for, so the tab can name them
 * and the editor can hold Save back: a list over its entry count or with an
 * entry over its length, a relationship line that is not `agent: stance`, and
 * `size` when the whole soul is over {@link SOUL_MAX_TEXT_BYTES}. The single
 * line fields carry `maxLength` on their inputs instead. Lengths are UTF-16
 * code units, which is at least the rune count Main measures, so a draft this
 * passes is never refused for a length.
 */
export function soulDraftProblems(draft: AgentSoulDraft): ReadonlySet<keyof AgentSoulDraft | 'size'> {
  const problems = new Set<keyof AgentSoulDraft | 'size'>();
  for (const key of Object.keys(LIST_BOUNDS) as (keyof typeof LIST_BOUNDS)[]) {
    if (listBroken(draft[key], LIST_BOUNDS[key])) problems.add(key);
  }
  if (relationshipsBroken(draft.relationships)) problems.add('relationships');
  if (soulTextBytes(soulFromDraft(draft)) > SOUL_MAX_TEXT_BYTES) problems.add('size');
  return problems;
}
