import type { AndroidSymbol, SFSymbol } from 'expo-symbols';

import { icons as tokenIcons } from '@/design/tokens';

/**
 * Every icon of the app as one semantic name: [SF Symbol (iOS), Material Symbol (Android)]
 * (docs/design-system.md §1.8). The design-token map (`design-tokens.json → icons.map`)
 * plus the names the kit and the shell need. Components take the semantic name only;
 * no screen spells a platform symbol name itself.
 */
const KIT_ICONS = {
  chevronRight: ['chevron.right', 'chevron_right'],
  chevronDown: ['chevron.down', 'expand_more'],
  check: ['checkmark', 'check'],
  checkCircle: ['checkmark.circle.fill', 'check_circle'],
  errorCircle: ['xmark.octagon.fill', 'cancel'],
  closeCircle: ['xmark.circle.fill', 'cancel'],
  pauseCircle: ['pause.circle', 'pause_circle'],
  plus: ['plus', 'add'],
  person: ['person.crop.circle', 'account_circle'],
  people: ['person.2', 'group'],
  info: ['info.circle', 'info'],
  warning: ['exclamationmark.triangle', 'warning'],
  error: ['exclamationmark.circle', 'error'],
  share: ['square.and.arrow.up', 'ios_share'],
  openExternal: ['arrow.up.forward.square', 'open_in_new'],
  light: ['sun.max', 'light_mode'],
  dark: ['moon', 'dark_mode'],
  system: ['circle.lefthalf.filled', 'contrast'],
  memory: ['brain', 'psychology'],
  clock: ['clock', 'schedule'],
  link: ['link', 'link'],
  palette: ['paintpalette', 'palette'],
  signOut: ['rectangle.portrait.and.arrow.right', 'logout'],
  switchWorkspace: ['arrow.left.arrow.right', 'swap_horiz'],
  sync: ['arrow.triangle.2.circlepath', 'sync'],
  usage: ['chart.bar', 'bar_chart'],
  fingerprint: ['faceid', 'fingerprint'],
  faceId: ['faceid', 'face'],
  touchId: ['touchid', 'fingerprint'],
  passcode: ['lock.shield', 'password'],
  grid: ['square.grid.2x2', 'apps'],
  keyboard: ['keyboard', 'keyboard'],
  arrowDown: ['arrow.down', 'arrow_downward'],
  document: ['doc', 'description'],
  code: ['chevron.left.forwardslash.chevron.right', 'code'],
  speakerOff: ['speaker.slash', 'volume_off'],
  retry: ['arrow.clockwise', 'refresh'],
  photoLibrary: ['photo.on.rectangle', 'photo_library'],
  list: ['list.bullet', 'view_list'],
  mention: ['at', 'alternate_email'],
  personAdd: ['person.badge.plus', 'person_add'],
  markAllRead: ['checkmark.circle', 'done_all'],
  phone: ['iphone', 'smartphone'],
  tablet: ['ipad', 'tablet'],
  devices: ['laptopcomputer.and.iphone', 'devices'],
  shield: ['checkmark.shield', 'verified_user'],
  gauge: ['gauge.with.dots.needle.50percent', 'speed'],
  cpu: ['cpu', 'memory'],
  sparkles: ['sparkles', 'auto_awesome'],
  bellOff: ['bell.slash', 'notifications_off'],
  version: ['number', 'tag'],
  rename: ['pencil', 'edit'],
  unpin: ['pin.slash', 'keep_off'],
} as const satisfies Record<string, readonly [SFSymbol, AndroidSymbol]>;

export const ICONS = { ...(tokenIcons as Record<keyof typeof tokenIcons, readonly [SFSymbol, AndroidSymbol]>), ...KIT_ICONS };

export type IconName = keyof typeof ICONS;

/**
 * The filled SF Symbol for a selected state (tabs, toggles), when one exists. Material
 * Symbols mark selection with fill 1 of the same glyph, which the bundled font does not
 * carry; Android shows selection with the indicator / tint instead.
 */
export function filledSymbol(name: IconName): SFSymbol {
  const sf = ICONS[name][0];
  const FILLABLE =
    /^(house|bell|gearshape|mic|hand\.thumbsup|hand\.thumbsdown|folder|lock|pin|archivebox|person\.crop\.circle|person\.2|info\.circle|exclamationmark\.triangle|exclamationmark\.circle|clock|paintpalette|square\.grid\.2x2|bubble\.left\.and\.bubble\.right|moon|sun\.max|trash|camera|photo|doc\.on\.doc|speaker\.wave\.2|building\.2)$/;
  return (FILLABLE.test(sf) && !sf.endsWith('.fill') ? `${sf}.fill` : sf) as SFSymbol;
}

/** The platform symbol of a semantic icon. */
export function symbolFor(name: IconName, platform: 'ios' | 'android', selected = false): string {
  return platform === 'ios' ? (selected ? filledSymbol(name) : ICONS[name][0]) : ICONS[name][1];
}
