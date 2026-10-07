/**
 * Agent Zefir design tokens v1 (docs/design-system.md, docs/design-tokens.json).
 *
 * This module is the typed copy of `docs/design-tokens.json`; `tokens.test.ts`
 * fails when the two drift and re-proves every WCAG pair in `PROOFS`. Screens
 * read these through `useDesign()` (src/design/DesignProvider.tsx), never
 * directly, so the workspace brand and accessibility modifiers apply.
 */

export type Scheme = 'light' | 'dark';

export const primitives = {
  marshmallow: {
    50: '#FFF4F8', 100: '#FCE4EE', 200: '#F8C7DA', 300: '#F2A6C3', 400: '#E77AA4',
    500: '#D04F86', 600: '#B4306A', 700: '#93244F', 800: '#6E1A3B', 900: '#3D1E2D',
  },
  cream: { 50: '#FFFCFA', 100: '#FBF7F9', 200: '#F5EFF3' },
  coat: {
    50: '#EEF6FB', 100: '#DDEBF5', 200: '#BFDDF3', 300: '#A8CCE6', 400: '#8DC3EA',
    500: '#5E9CC8', 600: '#2D6894', 700: '#1E4A6B', 800: '#1C2C3A',
  },
  mint: { 50: '#EEF9F4', 100: '#E8F5EF', 200: '#CBE9DA', 300: '#9ED9D0', 500: '#3FA68A', 700: '#1D7A4C' },
  belt: { 100: '#FCEBD6', 300: '#F0B98C', 400: '#E4A176', 600: '#B8652A', 700: '#8F5300' },
  ink: {
    0: '#FFFFFF', 50: '#F7F2F5', 100: '#E6DDE3', 300: '#BDB4C2', 400: '#9C93A2', 500: '#8E8592',
    550: '#77707E', 600: '#6E6676', 700: '#5A5262', 900: '#1C1622',
  },
  night: { 950: '#100E13', 900: '#141118', 850: '#1D1A22', 800: '#221F28', 750: '#26222C', 700: '#2A2631', 600: '#332E3A' },
} as const;

export interface Gradient {
  angleDeg: number;
  stops: readonly [string, string, string];
  positions: readonly [number, number, number];
}

export interface Glass {
  /** Fill colour; drawn at `alpha` over a blur of `blur` pt. */
  rgb: string;
  alpha: number;
  blur: number;
  border: string;
  /** Used when Reduce Transparency is on, or where blur is unavailable. */
  opaqueFallback: string;
}

export interface Orb {
  stops: readonly [string, string, string];
  highlight: string;
  /** Decorative only: never carries meaning, exempt from contrast. */
  decorative: true;
}

/** The semantic colour set. Every screen colour comes from here. */
export interface SemanticColors {
  background: string;
  backgroundGradient: Gradient;
  surface: string;
  surfaceSunken: string;
  surfaceGlass: Glass;
  elevated: string;
  textPrimary: string;
  textSecondary: string;
  textTertiary: string;
  separator: string;
  separatorStrong: string;
  accent: string;
  accentPressed: string;
  accentOn: string;
  accentSoft: string;
  accentSoftOn: string;
  secondary: string;
  secondarySoft: string;
  secondarySoftOn: string;
  success: string;
  successSoft: string;
  warning: string;
  warningSoft: string;
  danger: string;
  dangerOn: string;
  dangerSoft: string;
  bubbleUser: string;
  bubbleUserText: string;
  bubbleAgent: string;
  bubbleAgentText: string;
  bubbleAgentBorder: string;
  chip: string;
  chipText: string;
  chipSelected: string;
  chipSelectedText: string;
  badge: string;
  badgeText: string;
  focus: string;
  scrim: string;
  orb: Orb;
  skeleton: string;
}

/** Keys whose value is a plain colour string. */
export type ColorKey = {
  [K in keyof SemanticColors]: SemanticColors[K] extends string ? K : never;
}[keyof SemanticColors];

export const colors: Record<Scheme, SemanticColors> = {
  light: {
    background: '#FBF7F9',
    backgroundGradient: { angleDeg: 160, stops: ['#FCEBF2', '#F2EEFB', '#E8F5EF'], positions: [0, 0.55, 1] },
    surface: '#FFFFFF',
    surfaceSunken: '#F5EFF3',
    surfaceGlass: { rgb: '#FFFFFF', alpha: 0.72, blur: 24, border: 'rgba(255, 255, 255, 0.9)', opaqueFallback: '#FFFFFF' },
    elevated: '#FFFFFF',
    textPrimary: '#1C1622',
    textSecondary: '#5A5262',
    textTertiary: '#6E6676',
    separator: '#E6DDE3',
    separatorStrong: '#8E8592',
    accent: '#B4306A',
    accentPressed: '#93244F',
    accentOn: '#FFFFFF',
    accentSoft: '#FCE4EE',
    accentSoftOn: '#93244F',
    secondary: '#2D6894',
    secondarySoft: '#DDEBF5',
    secondarySoftOn: '#1E4A6B',
    success: '#1D7A4C',
    successSoft: '#E8F5EF',
    warning: '#8F5300',
    warningSoft: '#FCEBD6',
    danger: '#C0262D',
    dangerOn: '#FFFFFF',
    dangerSoft: '#FBE0E0',
    bubbleUser: '#B4306A',
    bubbleUserText: '#FFFFFF',
    bubbleAgent: '#FFFFFF',
    bubbleAgentText: '#1C1622',
    bubbleAgentBorder: '#E6DDE3',
    chip: '#FFFFFF',
    chipText: '#1C1622',
    chipSelected: '#FCE4EE',
    chipSelectedText: '#93244F',
    badge: '#C0262D',
    badgeText: '#FFFFFF',
    focus: '#2D6894',
    scrim: 'rgba(28, 22, 34, 0.40)',
    orb: { stops: ['#F7A8C8', '#C9B6F2', '#9ED9D0'], highlight: 'rgba(255, 255, 255, 0.85)', decorative: true },
    skeleton: '#EFE7EC',
  },
  dark: {
    background: '#141118',
    backgroundGradient: { angleDeg: 160, stops: ['#1F1520', '#16151F', '#111B19'], positions: [0, 0.55, 1] },
    surface: '#1D1A22',
    surfaceSunken: '#100E13',
    surfaceGlass: { rgb: '#2A2631', alpha: 0.72, blur: 24, border: 'rgba(255, 255, 255, 0.08)', opaqueFallback: '#26222C' },
    elevated: '#26222C',
    textPrimary: '#F7F2F5',
    textSecondary: '#BDB4C2',
    textTertiary: '#9C93A2',
    separator: '#332E3A',
    separatorStrong: '#77707E',
    accent: '#FF8CB8',
    accentPressed: '#FFA9CB',
    accentOn: '#3A0A1F',
    accentSoft: '#3D1E2D',
    accentSoftOn: '#FFC2DA',
    secondary: '#8DC3EA',
    secondarySoft: '#1C2C3A',
    secondarySoftOn: '#BFDDF3',
    success: '#5CD49C',
    successSoft: '#16291F',
    warning: '#F3B54A',
    warningSoft: '#2E2213',
    danger: '#FF8780',
    dangerOn: '#3A0705',
    dangerSoft: '#331717',
    bubbleUser: '#5B2541',
    bubbleUserText: '#FFFFFF',
    bubbleAgent: '#221F28',
    bubbleAgentText: '#F7F2F5',
    bubbleAgentBorder: '#332E3A',
    chip: '#26222C',
    chipText: '#F7F2F5',
    chipSelected: '#3D1E2D',
    chipSelectedText: '#FFC2DA',
    badge: '#D2333A',
    badgeText: '#FFFFFF',
    focus: '#8DC3EA',
    scrim: 'rgba(0, 0, 0, 0.55)',
    orb: { stops: ['#C2477E', '#6F5BB8', '#2F8C7E'], highlight: 'rgba(255, 255, 255, 0.55)', decorative: true },
    skeleton: '#2A2631',
  },
};

/**
 * WCAG pairs that must hold in both schemes: [foreground, background].
 * A `backgroundGradient` background means every stop; `surfaceGlass` means the
 * glass composited over every gradient stop and the flat background.
 */
export const PROOFS: readonly (readonly [ColorKey, ColorKey | 'backgroundGradient' | 'surfaceGlass'])[] = [
  ['textPrimary', 'background'], ['textPrimary', 'backgroundGradient'], ['textPrimary', 'surface'],
  ['textPrimary', 'surfaceGlass'], ['textPrimary', 'elevated'], ['textPrimary', 'surfaceSunken'],
  ['textSecondary', 'background'], ['textSecondary', 'backgroundGradient'], ['textSecondary', 'surface'],
  ['textSecondary', 'surfaceGlass'], ['textSecondary', 'elevated'],
  ['textTertiary', 'background'], ['textTertiary', 'backgroundGradient'], ['textTertiary', 'surface'],
  ['textTertiary', 'surfaceGlass'], ['textTertiary', 'elevated'],
  ['accent', 'background'], ['accent', 'backgroundGradient'], ['accent', 'surface'], ['accent', 'surfaceGlass'],
  ['accent', 'elevated'],
  ['accentOn', 'accent'], ['accentSoftOn', 'accentSoft'],
  ['secondary', 'surface'], ['secondary', 'backgroundGradient'], ['secondarySoftOn', 'secondarySoft'],
  ['success', 'surface'], ['warning', 'surface'], ['danger', 'surface'], ['danger', 'backgroundGradient'],
  ['dangerOn', 'danger'],
  ['bubbleUserText', 'bubbleUser'], ['bubbleAgentText', 'bubbleAgent'],
  ['chipText', 'chip'], ['chipText', 'surfaceGlass'], ['chipSelectedText', 'chipSelected'],
  ['badgeText', 'badge'],
  ['separatorStrong', 'surface'], ['separatorStrong', 'background'], ['separatorStrong', 'surfaceSunken'],
  ['focus', 'background'], ['focus', 'backgroundGradient'], ['focus', 'surface'],
];

/** Non-text UI (borders, focus rings) needs 3:1; everything else is text at 4.5:1. */
export const NON_TEXT_PROOF_KEYS: readonly ColorKey[] = ['separatorStrong', 'focus'];

export const space = {
  0: 0, 0.5: 2, 1: 4, 2: 8, 3: 12, 4: 16, 5: 20, 6: 24, 8: 32, 10: 40, 12: 48, 16: 64,
  screenGutter: 20,
  screenGutterCompact: 16,
  cardPadding: 16,
  cardGap: 12,
  sectionGap: 28,
  listRowPaddingY: 12,
  inlineGap: 8,
  composerInset: 12,
} as const;

export const radius = {
  xs: 6, sm: 10, md: 16, lg: 22, xl: 28, sheet: 32, pill: 9999,
  chip: 9999, button: 9999, card: 22, actionCard: 28, bubble: 22, bubbleTail: 6, composer: 28,
  listRow: 16, sheetTop: 32, avatar: 9999, orb: 9999,
} as const;

export interface Elevation {
  shadowColor: string;
  shadowOpacity: number;
  shadowRadius: number;
  shadowOffset: { width: number; height: number };
  /** Android `elevation`. */
  elevation: number;
}

function shadow(shadowColor: string, shadowOpacity: number, shadowRadius: number, offsetY: number, elevation: number): Elevation {
  return { shadowColor, shadowOpacity, shadowRadius, shadowOffset: { width: 0, height: offsetY }, elevation };
}

const NONE = shadow('#1C1622', 0, 0, 0, 0);

/**
 * e1 cards and chips on the gradient, e2 floating composer / FAB / popovers,
 * e3 sheets and menus. Dark mode leans on lighter surfaces instead; its
 * shadows are kept but barely visible.
 */
export const elevation: Record<Scheme, Record<'e0' | 'e1' | 'e2' | 'e3', Elevation>> = {
  light: {
    e0: NONE,
    e1: shadow('#6E1A3B', 0.06, 12, 4, 1),
    e2: shadow('#6E1A3B', 0.1, 24, 8, 3),
    e3: shadow('#1C1622', 0.16, 40, 16, 6),
  },
  dark: {
    e0: NONE,
    e1: shadow('#000000', 0.3, 12, 4, 1),
    e2: shadow('#000000', 0.4, 24, 8, 3),
    e3: shadow('#000000', 0.5, 40, 16, 6),
  },
};

/** Dark-mode top highlight that stands in for a shadow on raised surfaces. */
export const DARK_TOP_HIGHLIGHT = 'rgba(255, 255, 255, 0.06)';

export const size = {
  hitTarget: { ios: 44, android: 48 },
  iconInline: 20,
  iconButton: 22,
  iconTab: 24,
  circleButton: { visual: 40, compact: 36 },
  avatar: { xs: 24, sm: 28, md: 36, lg: 48, xl: 64 },
  composerMinHeight: 52,
  composerMaxLines: 6,
  orb: { compact: 160, regular: 220, max: 260 },
  maxContentWidth: 640,
} as const;

export const motion = {
  duration: { instant: 100, fast: 180, base: 260, slow: 400, orbBreath: 2400, reducedCrossFade: 150 },
  easing: {
    standard: [0.2, 0, 0, 1],
    emphasizedDecelerate: [0.05, 0.7, 0.1, 1],
    emphasizedAccelerate: [0.3, 0, 0.8, 0.15],
  },
  spring: {
    default: { damping: 20, stiffness: 220, mass: 1 },
    bouncy: { damping: 14, stiffness: 180, mass: 1 },
    snappy: { damping: 26, stiffness: 320, mass: 1 },
  },
} as const;

/**
 * Haptic per user-initiated event (expo-haptics, not yet a dependency).
 * Never on streaming tokens or incoming notifications.
 */
export const haptics = {
  send: 'impact.light',
  voiceStart: 'impact.medium',
  voiceStop: 'impact.light',
  copy: 'notification.success',
  feedbackLikeDislike: 'selection',
  swipeActionThreshold: 'impact.rigid',
  approvalApprove: 'notification.success',
  approvalReject: 'notification.warning',
  error: 'notification.error',
  tabOrChipChange: 'selection',
} as const;

/** [SF Symbol, Material Symbol] per role. */
export const icons = {
  home: ['house', 'home'], chats: ['bubble.left.and.bubble.right', 'forum'], discover: ['sparkles', 'explore'],
  settings: ['gearshape', 'settings'], bell: ['bell', 'notifications'], newChat: ['square.and.pencil', 'edit_square'],
  arrowGo: ['arrow.up.right', 'arrow_outward'], mic: ['mic', 'mic'], micFill: ['mic.fill', 'mic'],
  send: ['arrow.up', 'arrow_upward'], stop: ['stop.fill', 'stop'], attach: ['paperclip', 'attach_file'],
  image: ['photo', 'image'], camera: ['camera', 'photo_camera'], copy: ['doc.on.doc', 'content_copy'],
  like: ['hand.thumbsup', 'thumb_up'], dislike: ['hand.thumbsdown', 'thumb_down'],
  readAloud: ['speaker.wave.2', 'volume_up'], regenerate: ['arrow.clockwise', 'refresh'], more: ['ellipsis', 'more_vert'],
  back: ['chevron.backward', 'arrow_back'], close: ['xmark', 'close'], tool: ['wrench.and.screwdriver', 'build'],
  approve: ['checkmark', 'check'], reject: ['xmark', 'close'], lock: ['lock.fill', 'lock'],
  workspace: ['building.2', 'domain'], project: ['folder', 'folder'], qr: ['qrcode.viewfinder', 'qr_code_scanner'],
  delete: ['trash', 'delete'], pin: ['pin', 'keep'], archive: ['archivebox', 'archive'],
  offline: ['wifi.slash', 'wifi_off'], search: ['magnifyingglass', 'search'], agent: ['sparkle', 'smart_toy'],
} as const satisfies Record<string, readonly [string, string]>;

/** Tokens a workspace brand pack may change (docs/design-system.md §3.1). Everything else is fixed. */
export const BRAND_OVERRIDABLE = [
  'accent', 'accentPressed', 'accentOn', 'accentSoft', 'accentSoftOn',
  'bubbleUser', 'bubbleUserText', 'chipSelected', 'chipSelectedText',
] as const satisfies readonly ColorKey[];

export type BrandOverridableKey = (typeof BRAND_OVERRIDABLE)[number];

/** WCAG targets for brand correction. */
export const BRAND_TARGETS = { accentOnBackground: 4.5, accentOnAccent: 4.5, tintTertiary: 4.5 } as const;
