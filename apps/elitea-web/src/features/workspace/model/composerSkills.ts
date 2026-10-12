/**
 * The selected agent's skills in the composer's "/" menu, as pure text
 * functions: which skills match what follows the "/", and which skill a
 * message invokes (its first word is `/<skill name>`).
 *
 * Only the agent version's own skills are offered: they are what the
 * runtime holds for the turn (and what the host resolves the pick
 * against), as the web chat's "~" menu offers them.
 */

export interface ComposerSkill {
  id: string;
  name: string;
  description?: string | undefined;
}

/** How many skills the menu lists at most. */
const SKILL_LIMIT = 50;

/**
 * The skills whose name holds `query` (case-insensitive): names starting
 * with it first, then the rest, each alphabetically; at most 50.
 */
export function matchingSkills(skills: readonly ComposerSkill[], query: string): ComposerSkill[] {
  const wanted = query.toLowerCase();
  const rank = (skill: ComposerSkill): number => (skill.name.toLowerCase().startsWith(wanted) ? 0 : 1);
  return skills
    .filter((skill) => skill.name.toLowerCase().includes(wanted))
    .sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name))
    .slice(0, SKILL_LIMIT);
}

/** What the composer inserts for a picked skill. */
export function skillToken(skill: ComposerSkill): string {
  return `/${skill.name}`;
}

/**
 * The skill `text` invokes: the message (leading blanks aside) starts with
 * `/<name>` followed by whitespace or its end, case-insensitively; the
 * longest such name wins. `null` when it invokes none.
 */
export function invokedSkill(text: string, skills: readonly ComposerSkill[]): ComposerSkill | null {
  const message = text.trimStart().toLowerCase();
  let best: ComposerSkill | null = null;
  for (const skill of skills) {
    const token = skillToken(skill).toLowerCase();
    const next = message.charAt(token.length);
    if (message.startsWith(token) && (next === '' || /\s/u.test(next)) && (best === null || skill.name.length > best.name.length)) best = skill;
  }
  return best;
}
