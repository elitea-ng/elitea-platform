/**
 * Version-name helpers for "Save as new version" (#2102, #2103).
 *
 * `base` is the reserved first version of every skill, so the first name
 * offered is `v1`. Otherwise the suggestion is the highest numbered `vN`
 * (or bare `N`) already on the skill, plus one.
 */
const NUMBERED_VERSION = /^(v?)(\d+)$/i;

/** The name that "Save as new version" offers: never one the skill already has. */
export function nextVersionName(existingNames: readonly string[]): string {
  let highest = 0;
  let prefix = 'v';
  for (const name of existingNames) {
    const match = NUMBERED_VERSION.exec(name.trim());
    if (match === null) continue;
    const number = Number(match[2]);
    if (number >= highest) {
      highest = number;
      prefix = match[1] ?? 'v';
    }
  }
  const taken = new Set(existingNames.map((name) => name.trim().toLowerCase()));
  let candidate = highest + 1;
  while (taken.has(`${prefix}${candidate}`.toLowerCase())) candidate += 1;
  return `${prefix}${candidate}`;
}

/** True when `name` collides with an existing version (case-insensitive, trimmed). */
export function isDuplicateVersionName(name: string, existingNames: readonly string[]): boolean {
  const wanted = name.trim().toLowerCase();
  return wanted !== '' && existingNames.some((existing) => existing.trim().toLowerCase() === wanted);
}
