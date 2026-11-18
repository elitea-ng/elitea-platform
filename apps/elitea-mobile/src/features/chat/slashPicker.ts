/**
 * The "/" picker of a model chat, in two phases over one token:
 *
 *   "/gi"            phase 1: the conversation's toolkits, filtered by "gi"
 *   "/github/"       phase 2: that toolkit's tools
 *   "/github/list"   phase 2, filtered by "list"
 *
 * A toolkit pick inserts "/<Toolkit>/" (the token stays open for phase 2);
 * a tool pick inserts "/<Toolkit>/<tool> ". Text only: nothing changes in
 * what is sent besides the words.
 *
 * Skills (ADR-0050 amendment 1, A1.5): phase 1 opens with one first entry, "Skills", and
 * "/skill <filter>" (typed or reached through that entry) is the skill phase: the project's
 * skills, those pinned to the conversation first. A pick inserts the "~name" token, the one
 * wire syntax, so a message never carries the text "/skill".
 */

export interface ToolkitOption {
  /** The toolkit's id (`entity_meta.id`), for listToolkitAvailableTools. */
  toolkitId: number;
  name: string;
}

export type SlashPhase =
  | { phase: 'toolkit'; filter: string }
  /** "/skill <filter>": the project's skills, the conversation's pinned ones first. */
  | { phase: 'skill'; filter: string }
  | { phase: 'tool'; toolkit: ToolkitOption; filter: string }
  /** The text before the second slash names no toolkit of this conversation. */
  | { phase: 'unknown_toolkit'; name: string };

/** "skill", then a space or tab, then the filter (no whitespace): the query of the skill phase. */
const SKILL_COMMAND = /^skill[ \t]+(\S*)$/i;

export function slashPhase(query: string, toolkits: readonly ToolkitOption[]): SlashPhase {
  const command = SKILL_COMMAND.exec(query);
  if (command) return { phase: 'skill', filter: command[1] };
  const slash = query.indexOf('/');
  if (slash < 0) return { phase: 'toolkit', filter: query };
  const name = query.slice(0, slash);
  const toolkit = toolkits.find((t) => t.name.toLocaleLowerCase() === name.toLocaleLowerCase());
  if (!toolkit) return { phase: 'unknown_toolkit', name };
  return { phase: 'tool', toolkit, filter: query.slice(slash + 1) };
}

/** A token-safe spelling of a name (no whitespace or slashes inside the token). */
export function tokenName(name: string): string {
  return name.trim().replace(/[\s/]+/g, '_');
}

export function matches(name: string, filter: string): boolean {
  return name.toLocaleLowerCase().includes(filter.toLocaleLowerCase());
}

export function toolkitInsert(toolkit: ToolkitOption): string {
  return `/${tokenName(toolkit.name)}/`;
}

export function toolInsert(toolkit: ToolkitOption, tool: string): string {
  return `/${tokenName(toolkit.name)}/${tokenName(tool)} `;
}

export function skillInsert(skill: string): string {
  return `~${tokenName(skill)} `;
}

/** The key of phase 1's first entry, which opens the skill phase. */
export const SKILLS_ENTRY_KEY = 'skills-entry';

/** What the "Skills" entry inserts: the command, left open for the filter. */
export const SKILLS_COMMAND = '/skill ';

/** Phase 1 lists the Skills entry while what was typed so far is a start of "skill" or "skills". */
export function skillsEntryMatches(filter: string): boolean {
  const typed = filter.trim().toLocaleLowerCase();
  return typed === '' || 'skills'.startsWith(typed);
}

export interface SkillChoice {
  id: number;
  name: string;
  description: string;
}

export interface SkillPinState {
  /** The project skill's id (a listing pin has none and is not offered here). */
  skillId: number;
  untrusted: boolean;
}

export interface SkillRow {
  key: string;
  title: string;
  subtitle: string;
  /** "Pinned", "Pinned, untrusted" or null. */
  badge: string | null;
}

/** The skill phase's rows: the filter applied, pinned skills first, each group by name. */
export function skillRows(options: readonly SkillChoice[], pins: readonly SkillPinState[], filter: string): SkillRow[] {
  const pinned = new Map(pins.map((pin) => [pin.skillId, pin]));
  return options
    .filter((option) => matches(option.name, filter))
    .sort((left, right) => Number(pinned.has(right.id)) - Number(pinned.has(left.id)) || left.name.localeCompare(right.name))
    .map((option) => {
      const pin = pinned.get(option.id);
      return {
        key: `skill-${option.id}`,
        title: option.name,
        subtitle: option.description || 'Skill',
        badge: pin === undefined ? null : pin.untrusted ? 'Pinned, untrusted' : 'Pinned',
      };
    });
}
