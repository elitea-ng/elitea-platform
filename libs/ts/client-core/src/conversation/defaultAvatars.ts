/**
 * The default sub-agent avatars (ADR-0050 decisions 4 and 6, issues 1870 and 1951).
 *
 * A `spawn_agent` profile names its avatar by a key, `default:<name>`. The
 * set of names is CLOSED: the agent runtime refuses any other `default:`
 * key at spawn (`libs/rust/agent-runtime/src/subagents/profile.rs` keeps the
 * same names, and a test reads the `DEFAULT_AVATAR_NAMES` line below, so the
 * two cannot drift). Each name has ONE image here, drawn as small geometric
 * shapes, so the web, the desktop (the web build) and the mobile app all draw
 * the same animal from the same data.
 *
 * An image is data, not a file: a few paths on a 24 by 24 grid. Each path has
 * a colour ROLE, never a colour. The client maps a role to its own theme
 * token, so a white-label brand and the dark scheme re-theme the avatars.
 *
 * An unknown key has no image: the client draws the brand mark.
 */

/** The key prefix of a default avatar. */
export const DEFAULT_AVATAR_PREFIX = 'default:';

/** The names, in display order. The Rust runtime reads this line: keep it on one line. */
export const DEFAULT_AVATAR_NAMES = ['owl', 'hawk', 'fox', 'wolf', 'bear', 'otter', 'heron', 'lynx'] as const;

export type DefaultAvatarName = (typeof DEFAULT_AVATAR_NAMES)[number];

/** `body` is the animal's colour, `detail` the contrast of its eyes, `accent` the muted rest (beak, nose). */
export type DefaultAvatarRole = 'body' | 'detail' | 'accent';

export interface DefaultAvatarShape {
  readonly role: DefaultAvatarRole;
  /** SVG path data on the shared grid. */
  readonly d: string;
}

export interface DefaultAvatar {
  readonly name: DefaultAvatarName;
  /** The accessible name of the image. */
  readonly label: string;
  readonly shapes: readonly DefaultAvatarShape[];
}

/** The side of the square grid every image is drawn on. */
export const DEFAULT_AVATAR_GRID = 24;

/** A filled circle as path data. */
const dot = (cx: number, cy: number, r: number): string =>
  `M${String(cx - r)} ${String(cy)}a${String(r)} ${String(r)} 0 1 0 ${String(2 * r)} 0a${String(r)} ${String(r)} 0 1 0 ${String(-2 * r)} 0Z`;

export const DEFAULT_AVATARS: Readonly<Record<DefaultAvatarName, DefaultAvatar>> = {
  owl: {
    name: 'owl',
    label: 'Owl',
    shapes: [
      { role: 'body', d: 'M4 4l4 3h8l4-3v11a8 8 0 0 1-16 0Z' },
      { role: 'detail', d: dot(8.5, 11, 3) },
      { role: 'detail', d: dot(15.5, 11, 3) },
      { role: 'body', d: dot(8.5, 11, 1.2) },
      { role: 'body', d: dot(15.5, 11, 1.2) },
      { role: 'accent', d: 'M12 13.5l-1.8 3h3.6Z' },
    ],
  },
  hawk: {
    name: 'hawk',
    label: 'Hawk',
    shapes: [
      { role: 'body', d: 'M3 7l9-3 9 3-2 6-7 8-7-8Z' },
      { role: 'detail', d: 'M6.5 9.5l4 1-1.5 2.2Zm11 0l-4 1 1.5 2.2Z' },
      { role: 'accent', d: 'M12 11.5l-2.5 2.5 2.5 5 2.5-5Z' },
    ],
  },
  fox: {
    name: 'fox',
    label: 'Fox',
    shapes: [
      { role: 'body', d: 'M3 3l6 4h6l6-4-1 10-8 9-8-9Z' },
      { role: 'detail', d: 'M12 22l-4.5-5.5h9Z' },
      { role: 'detail', d: dot(8.5, 11.5, 1.3) },
      { role: 'detail', d: dot(15.5, 11.5, 1.3) },
      { role: 'accent', d: dot(12, 19.5, 1.2) },
    ],
  },
  wolf: {
    name: 'wolf',
    label: 'Wolf',
    shapes: [
      { role: 'body', d: 'M4 2l5 5h6l5-5 1 11-5 5v4h-8v-4l-5-5Z' },
      { role: 'detail', d: 'M7.5 11l3 1.2-3 1.3Zm9 0l-3 1.2 3 1.3Z' },
      { role: 'accent', d: 'M10 18h4l-2 2.5Z' },
    ],
  },
  bear: {
    name: 'bear',
    label: 'Bear',
    shapes: [
      { role: 'body', d: dot(5.5, 5.5, 3) },
      { role: 'body', d: dot(18.5, 5.5, 3) },
      { role: 'body', d: dot(12, 13, 8.5) },
      { role: 'detail', d: dot(9, 11.5, 1.3) },
      { role: 'detail', d: dot(15, 11.5, 1.3) },
      { role: 'accent', d: 'M9 14.5a3 2.2 0 1 0 6 0a3 2.2 0 1 0 -6 0Z' },
    ],
  },
  otter: {
    name: 'otter',
    label: 'Otter',
    shapes: [
      { role: 'body', d: 'M12 3a9 9 0 0 1 9 9c0 5-4.5 9-9 9s-9-4-9-9a9 9 0 0 1 9-9Z' },
      { role: 'detail', d: 'M5 14a7 4.5 0 0 1 14 0 7 6 0 0 1-14 0Z' },
      { role: 'body', d: dot(9, 10.5, 1.3) },
      { role: 'body', d: dot(15, 10.5, 1.3) },
      { role: 'accent', d: 'M10.2 13h3.6l-1.8 2Z' },
    ],
  },
  heron: {
    name: 'heron',
    label: 'Heron',
    shapes: [
      { role: 'body', d: 'M9 2h5a3 3 0 0 1 3 3v5l-3 4v8h-3v-8L8 10V4Z' },
      { role: 'detail', d: dot(13, 6, 1.1) },
      { role: 'accent', d: 'M17 5l5 1.5-5 1.5Z' },
    ],
  },
  lynx: {
    name: 'lynx',
    label: 'Lynx',
    shapes: [
      { role: 'body', d: 'M5 1l3 6h8l3-6 2 13-6 8h-6l-6-8Z' },
      { role: 'detail', d: 'M7.5 12l3.5 1-3.5 1.5Zm9 0L13 13l3.5 1.5Z' },
      { role: 'accent', d: 'M10.5 17h3l-1.5 2Z' },
    ],
  },
};

/** The key of a default avatar name. */
export function defaultAvatarKey(name: DefaultAvatarName): string {
  return `${DEFAULT_AVATAR_PREFIX}${name}`;
}

/** The keys of every default avatar, in display order. */
export const DEFAULT_AVATAR_KEYS: readonly string[] = DEFAULT_AVATAR_NAMES.map(defaultAvatarKey);

/**
 * The image of an avatar key, or `undefined` for anything that is not a
 * default key of the set (`undefined`, another prefix, an unknown name): the
 * client then draws the brand mark.
 */
export function defaultAvatarOf(key: string | null | undefined): DefaultAvatar | undefined {
  if (typeof key !== 'string' || !key.startsWith(DEFAULT_AVATAR_PREFIX)) return undefined;
  const name = key.slice(DEFAULT_AVATAR_PREFIX.length);
  return (DEFAULT_AVATAR_NAMES as readonly string[]).includes(name) ? DEFAULT_AVATARS[name as DefaultAvatarName] : undefined;
}
