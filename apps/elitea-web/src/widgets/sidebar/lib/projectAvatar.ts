/**
 * Ported verbatim from `[fsd]/widgets/sidebar-root/lib/{constants/
 * projectAvatar.constants.js, helpers/projectAvatar.helpers.js}`.
 *
 * R-T1 (`elitea/no-raw-color`) is disabled per literal below, deliberately:
 * this is a fixed, decorative, per-initial-letter avatar palette — same
 * class of thing as GitHub/Slack's deterministic avatar colours — not a
 * themed UI surface. It was a hard-coded, non-brand JS constant in the old
 * app too (never read from `lightPalette.js`/`darkPalette.js`), is
 * identical in both colour schemes by design (an avatar's colour must stay
 * recognisable across a scheme toggle), and is NOT part of
 * `shared/brand/tokens/` (out of this unit's ownership fence to add to,
 * and semantically the wrong place for a decorative constant unrelated to
 * white-labeling).
 */
// Brand guide v3: neutrals carry the interface and orange is only the dot, so
// the per-letter hues (an orange, a purple, a teal…) became six Ink slate
// steps. Each carries the white initial at 5:1 or better, in both schemes.
const PROJECT_AVATAR_COLORS: ReadonlyArray<{ letters: string; color: string }> = [
  // oxlint-disable-next-line elitea/no-raw-color -- see file header: fixed decorative avatar palette, not a themed token.
  { letters: 'ABCD', color: '#3A4466' },
  // oxlint-disable-next-line elitea/no-raw-color -- see file header.
  { letters: 'EFGH', color: '#4A5578' },
  // oxlint-disable-next-line elitea/no-raw-color -- see file header.
  { letters: 'IJKL', color: '#2E3654' },
  // oxlint-disable-next-line elitea/no-raw-color -- see file header.
  { letters: 'MNOP', color: '#535D7A' },
  // oxlint-disable-next-line elitea/no-raw-color -- see file header.
  { letters: 'QRST', color: '#444E6E' },
  // oxlint-disable-next-line elitea/no-raw-color -- see file header.
  { letters: 'UVWXYZ', color: '#5A6478' },
];

// oxlint-disable-next-line elitea/no-raw-color -- see file header: fixed decorative fallback, not a themed token.
const DEFAULT_COLOR = '#232A45';

export function projectAvatarColor(projectName: string | undefined): string {
  const letter = (projectName ?? '')[0]?.toUpperCase();
  if (!letter) return DEFAULT_COLOR;
  const group = PROJECT_AVATAR_COLORS.find((entry) => entry.letters.includes(letter));
  return group?.color ?? DEFAULT_COLOR;
}

export function projectInitial(projectName: string | undefined): string {
  return (projectName ?? '?')[0]?.toUpperCase() ?? '?';
}
