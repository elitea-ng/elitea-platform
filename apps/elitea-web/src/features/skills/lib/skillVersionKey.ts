import type { SkillVersion } from '../model/types';

/**
 * The stable key one skill version is addressed by in the UI — a numeric id
 * when the wire carries one, else the version's name (a skill freshly
 * created client-side, before its first save, has versions with no id yet).
 *
 * Lives in `features/skills/lib` (not `pages/skills`) so both the editor
 * page and `features/skills/ui/SkillCompareModal.tsx` can use the same
 * function without a feature importing from a page — `pages/skills/
 * EditSkill.tsx` re-exports it for its own existing test's import path.
 */
export function skillVersionKey(version: SkillVersion): string {
  return String(version.id ?? version.name);
}
