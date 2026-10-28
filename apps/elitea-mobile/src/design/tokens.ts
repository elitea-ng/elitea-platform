import { BRAND_TOKENS } from '@elitea/client-core/brand';

/**
 * Elitea AI design tokens v2: the brand guide v3 palette (docs/design-system.md,
 * docs/design-tokens.json).
 *
 * The colours come from `BRAND_TOKENS` (`@elitea/client-core/brand`, the one source the web and
 * desktop apps read too); this module maps them onto the app's semantic set. It is the typed copy
 * of `docs/design-tokens.json`; `tokens.test.ts` fails when the two drift and re-proves every WCAG
 * pair in `PROOFS`. Screens read these through `useDesign()` (src/design/DesignProvider.tsx), never
 * directly, so the workspace brand and accessibility modifiers apply.
 */

export type Scheme = 'light' | 'dark';

/** The brand palette (guide v3): one job each, see `BRAND_TOKENS.palette`. */
export const primitives = {
  indigo: BRAND_TOKENS.palette.indigo.value,
  cyan: BRAND_TOKENS.palette.cyan.value,
  violet: BRAND_TOKENS.palette.violet.value,
  orange: BRAND_TOKENS.palette.orange.value,
  ink: BRAND_TOKENS.palette.ink.value,
  paper: BRAND_TOKENS.palette.paper.value,
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
  /** The one primary action fill (filled buttons, send): indigo in both schemes. */
  primary: string;
  primaryPressed: string;
  primaryOn: string;
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
  /** AI presence only (model and agent markers); `aiText` is its text-safe shade. */
  ai: string;
  aiText: string;
  /** The dot: one unread or live marker per view, never a fill for text. */
  dot: string;
  scrim: string;
  orb: Orb;
  skeleton: string;
}

/** Keys whose value is a plain colour string. */
export type ColorKey = {
  [K in keyof SemanticColors]: SemanticColors[K] extends string ? K : never;
}[keyof SemanticColors];

const DARK = BRAND_TOKENS.schemes.dark;
const LIGHT = BRAND_TOKENS.schemes.light;

/** No gradients (brand guide v3): the ground is flat, every stop is the background. */
function flat(color: string): Gradient {
  return { angleDeg: 180, stops: [color, color, color], positions: [0, 0.5, 1] };
}

/**
 * Brand v3 tokens per scheme. Indigo is the one primary fill; the signal colour (cyan on dark,
 * indigo on light) is `accent`, the tint of links, focus, active controls and spinners; violet
 * marks AI only; orange is the dot only; neutrals (Ink / Paper surfaces) carry everything else.
 * The tinted "soft" fills are the scheme's colour at the alpha of `BRAND_TOKENS.schemes.*.selected`
 * composited over the background, so they stay opaque for the proofs.
 */
export const colors: Record<Scheme, SemanticColors> = {
  light: {
    background: LIGHT.bg,
    backgroundGradient: flat(LIGHT.bg),
    surface: LIGHT.surface,
    surfaceSunken: '#EEF1F7',
    surfaceGlass: { rgb: LIGHT.elevated, alpha: 0.72, blur: 24, border: 'rgba(11, 11, 24, 0.08)', opaqueFallback: LIGHT.elevated },
    elevated: LIGHT.elevated,
    textPrimary: LIGHT.text,
    textSecondary: LIGHT.textMuted,
    textTertiary: LIGHT.textMuted,
    separator: LIGHT.border,
    separatorStrong: LIGHT.borderStrong,
    accent: LIGHT.signal,
    accentPressed: LIGHT.primaryHover,
    accentOn: LIGHT.onSignal,
    accentSoft: '#EDEDFD',
    accentSoftOn: LIGHT.primaryHover,
    primary: LIGHT.primary,
    primaryPressed: LIGHT.primaryPressed,
    primaryOn: LIGHT.onPrimary,
    secondary: LIGHT.textMuted,
    secondarySoft: '#E8EBF3',
    secondarySoftOn: LIGHT.text,
    success: LIGHT.success,
    successSoft: '#E8F2EC',
    warning: LIGHT.warning,
    warningSoft: '#FCF6F1',
    danger: LIGHT.danger,
    dangerOn: LIGHT.onDangerFill,
    dangerSoft: '#F8E8E8',
    bubbleUser: LIGHT.primary,
    bubbleUserText: LIGHT.onPrimary,
    bubbleAgent: LIGHT.surface,
    bubbleAgentText: LIGHT.text,
    bubbleAgentBorder: LIGHT.border,
    chip: LIGHT.elevated,
    chipText: LIGHT.text,
    chipSelected: '#EDEDFD',
    chipSelectedText: LIGHT.primaryHover,
    badge: LIGHT.dangerFill,
    badgeText: LIGHT.onDangerFill,
    focus: LIGHT.signal,
    ai: LIGHT.ai,
    aiText: LIGHT.aiText,
    dot: LIGHT.dot,
    scrim: LIGHT.scrim,
    orb: { stops: [LIGHT.signal, LIGHT.signal, LIGHT.signal], highlight: LIGHT.signal, decorative: true },
    skeleton: '#EEF1F7',
  },
  dark: {
    background: DARK.bg,
    backgroundGradient: flat(DARK.bg),
    surface: DARK.surface,
    surfaceSunken: DARK.codeBg,
    surfaceGlass: { rgb: DARK.elevated, alpha: 0.72, blur: 24, border: 'rgba(242, 244, 250, 0.08)', opaqueFallback: DARK.elevated },
    elevated: DARK.elevated,
    textPrimary: DARK.text,
    textSecondary: DARK.textMuted,
    textTertiary: DARK.textMuted,
    separator: DARK.border,
    separatorStrong: DARK.borderStrong,
    accent: DARK.signal,
    accentPressed: '#4DD2EE',
    accentOn: DARK.onSignal,
    accentSoft: '#191A46',
    accentSoftOn: DARK.signal,
    primary: DARK.primary,
    primaryPressed: DARK.primaryPressed,
    primaryOn: DARK.onPrimary,
    secondary: DARK.textMuted,
    secondarySoft: DARK.border,
    secondarySoftOn: DARK.text,
    success: DARK.success,
    successSoft: '#0E2521',
    warning: DARK.warning,
    warningSoft: '#2C2016',
    danger: DARK.danger,
    dangerOn: DARK.bg,
    dangerSoft: '#2C1925',
    bubbleUser: DARK.primary,
    bubbleUserText: DARK.onPrimary,
    bubbleAgent: DARK.surface,
    bubbleAgentText: DARK.text,
    bubbleAgentBorder: DARK.border,
    chip: DARK.elevated,
    chipText: DARK.text,
    chipSelected: '#191A46',
    chipSelectedText: DARK.signal,
    badge: DARK.dangerFill,
    badgeText: DARK.onDangerFill,
    focus: DARK.signal,
    ai: DARK.ai,
    aiText: DARK.aiText,
    dot: DARK.dot,
    scrim: DARK.scrim,
    orb: { stops: [DARK.signal, DARK.signal, DARK.signal], highlight: DARK.signal, decorative: true },
    skeleton: DARK.elevated,
  },
};

/**
 * WCAG pairs that must hold in both schemes: [foreground, background].
 * A `backgroundGradient` background means every stop; `surfaceGlass` means the
 * glass composited over every gradient stop and the flat background.
 */
/**
 * A proof's background: a token, or a composite ground (the gradient's stops, glass over every
 * ground, or a file chip's veil over the user's own bubble, src/design/contrast.ts).
 */
export type ProofGround = ColorKey | 'backgroundGradient' | 'surfaceGlass' | 'bubbleUserChip';

export const PROOFS: readonly (readonly [ColorKey, ProofGround])[] = [
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
  // The chat redesign: tool-step cards and code blocks (sunken), error rows and approval chrome.
  ['textSecondary', 'surfaceSunken'], ['textTertiary', 'surfaceSunken'], ['danger', 'dangerSoft'], ['warning', 'warningSoft'],
  ['accent', 'surfaceSunken'],
  // A file chip on my own bubble (AttachmentStrip): its name and type caption on the veiled bubble.
  ['bubbleUserText', 'bubbleUserChip'],
  // Brand v3: the primary fill's label, AI text on every ground it appears on, AI marks (non-text).
  ['primaryOn', 'primary'], ['aiText', 'background'], ['aiText', 'surface'], ['aiText', 'elevated'],
  ['aiText', 'secondarySoft'], ['ai', 'background'],
];

/** Non-text UI (borders, focus rings) needs 3:1; everything else is text at 4.5:1. */
export const NON_TEXT_PROOF_KEYS: readonly ColorKey[] = ['separatorStrong', 'focus', 'ai'];

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

const NONE = shadow(primitives.ink, 0, 0, 0, 0);

/**
 * e1 cards and chips on the gradient, e2 floating composer / FAB / popovers,
 * e3 sheets and menus. Dark mode leans on lighter surfaces instead; its
 * shadows are kept but barely visible.
 */
export const elevation: Record<Scheme, Record<'e0' | 'e1' | 'e2' | 'e3', Elevation>> = {
  light: {
    e0: NONE,
    e1: shadow(primitives.ink, 0.06, 12, 4, 1),
    e2: shadow(primitives.ink, 0.1, 24, 8, 3),
    e3: shadow(primitives.ink, 0.16, 40, 16, 6),
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
  'accent', 'accentPressed', 'accentOn', 'accentSoft', 'accentSoftOn', 'primary', 'primaryPressed', 'primaryOn',
  'bubbleUser', 'bubbleUserText', 'chipSelected', 'chipSelectedText',
] as const satisfies readonly ColorKey[];

export type BrandOverridableKey = (typeof BRAND_OVERRIDABLE)[number];

/** WCAG targets for brand correction. */
export const BRAND_TARGETS = { accentOnBackground: 4.5, accentOnAccent: 4.5, tintTertiary: 4.5 } as const;
