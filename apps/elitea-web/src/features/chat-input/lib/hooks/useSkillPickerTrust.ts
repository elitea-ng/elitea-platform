/**
 * Which of the skills a picker lists are untrusted (ADR-0051 amendment 1, A1.6), so the list can say so before a
 * person picks one: the text of a skill written in another project reaches the model as quoted data until this
 * project trusts it.
 *
 * A pinned skill carries its own trust state in the pin; any other project skill carries it in its `listSkills` row
 * (`trust`, client contract 1.63), so the picker asks nothing per row. A skill whose state is not known (a server
 * before 1.63 sends none) is not marked: the badge says what the server said, never a guess.
 */
import type { ConversationSkillPin } from '@/entities/conversation-skill';

import type { ProjectSkillEntry } from './useProjectSkillList';

/** The ids of the skills whose text is untrusted, among the conversation's `pins` and the project's `entries`. */
export function untrustedSkillIds(pins: readonly ConversationSkillPin[], entries: readonly ProjectSkillEntry[]): ReadonlySet<string> {
  const untrusted = new Set<string>();
  const pinned = new Set<string>();
  for (const pin of pins) {
    if (pin.skill_id === undefined) continue;
    pinned.add(String(pin.skill_id));
    if (pin.trust.state === 'untrusted') untrusted.add(String(pin.skill_id));
  }
  for (const entry of entries) if (!pinned.has(entry.id) && entry.untrusted === true) untrusted.add(entry.id);
  return untrusted;
}
