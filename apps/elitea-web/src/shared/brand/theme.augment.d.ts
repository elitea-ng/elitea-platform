/**
 * Hand-written module augmentation for the non-palette half of the theme
 * (the palette half is GENERATED into `tokens/palette.augment.d.ts`).
 *
 * Everything here exists because MUI's option interfaces are closed: without
 * the declaration the corresponding `createTheme` field is an excess-property
 * error, and the read site (`theme.typography.labelSmall`,
 * `theme.vars.shape.radiusMd`) has no type.
 */
import '@mui/material/styles';
import '@mui/material/Button';
import '@mui/material/Typography';

declare module '@mui/material/styles' {
  /**
   * The switch that makes `Theme` the CSS-VARIABLE theme type app-wide
   * (`createThemeNoVars.d.ts`: `CssThemeVariables extends { enabled: true }`
   * promotes `vars`, `colorSchemes`, `cssVarPrefix`, `generateStyleSheets` &
   * co. from optional to required). Without it every `theme.vars.…` read
   * needs a non-null assertion, and `ReturnType<typeof buildEliteaTheme>`
   * silently degrades to the no-vars `Theme`.
   *
   * It is correct app-wide because there is exactly one theme and
   * `cssVariables` is always on (buildTheme.ts).
   */
  interface CssThemeVariables {
    enabled: true;
  }

  /**
   * The eight variants of the one type scale (`shared/brand/typography.ts`):
   * four sizes, shared by the app and the admin console. Every MUI stock
   * variant still exists on `theme.typography` (MUI renders them internally)
   * but is aliased onto one of these; call sites may not name them (see
   * `TypographyPropsVariantOverrides` below and R-T13).
   */
  interface TypographyVariants {
    headingLarge: React.CSSProperties;
    headingMedium: React.CSSProperties;
    headingSmall: React.CSSProperties;
    labelMedium: React.CSSProperties;
    labelSmall: React.CSSProperties;
    bodyMedium: React.CSSProperties;
    bodySmall: React.CSSProperties;
    subtitle: React.CSSProperties;
    /** Pack field `typography.fontFamilyMono`; MUI has no built-in slot. */
    fontFamilyMono: string;
  }

  interface TypographyVariantsOptions {
    headingLarge?: React.CSSProperties;
    headingMedium?: React.CSSProperties;
    headingSmall?: React.CSSProperties;
    labelMedium?: React.CSSProperties;
    labelSmall?: React.CSSProperties;
    bodyMedium?: React.CSSProperties;
    bodySmall?: React.CSSProperties;
    subtitle?: React.CSSProperties;
    fontFamilyMono?: string;
  }

  /** `shape.radius*` — the only radii R-T10 allows (see buildTheme.ts). */
  interface ShapeOptions {
    radiusSm?: number;
    radiusMd?: number;
    radiusLg?: number;
    /** [S1 Part B] Additive — the pill/circle escape hatch (see buildTheme.ts). */
    radiusPill?: number;
  }

  /** MainTheme.js:92-111 — ten prompt-list rungs plus `tablet`. */
  interface BreakpointOverrides {
    prompt_list_xs: true;
    prompt_list_sm: true;
    prompt_list_full_width_sm: true;
    prompt_list_md: true;
    prompt_list_lg: true;
    prompt_list_xl: true;
    prompt_list_xxl: true;
    prompt_list_xxxl: true;
    prompt_list_xxxxl: true;
    prompt_list_xxxxxl: true;
    tablet: true;
  }
}

declare module '@mui/material/Button' {
  /**
   * The baseline's button variant vocabulary (`BaseBtn.jsx:12-27`). T1 wires
   * the six that carried the §4.1 Blocker-1 hard-coded accents; the names are
   * declared in full so unit S1 adds styles, not types.
   */
  interface ButtonPropsVariantOverrides {
    elitea: true;
    secondary: true;
    special: true;
    alarm: true;
    auxiliary: true;
    icon: true;
    iconCounter: true;
    maxi: true;
    iconLabel: true;
    tertiary: true;
    neutral: true;
    positive: true;
  }

  /**
   * [S1 Part B] Additive-only (no existing member touched). The baseline's
   * `BUTTON_COLORS` vocabulary (`BaseBtn.jsx:5-10`) beyond MUI's built-in
   * `primary`/`secondary`/`success`/`error`/`info`/`warning` — needed for
   * `variant="elitea" color="tertiary"|"alarm"` (`MuiButton.ts`'s per-colour
   * `elitea` entries) and for `shared/ui` callers (e.g.
   * `CopyToClipboardButton`) that pass those colours straight through to
   * `BaseBtn`/`Button`. Mirrors the `IconButtonPropsColorOverrides`
   * augmentation below it for the sibling `IconButton` component.
   */
  interface ButtonPropsColorOverrides {
    tertiary: true;
    alarm: true;
  }
}

declare module '@mui/material/Typography' {
  /**
   * `<Typography variant="labelSmall">` — the eight variants declared above.
   *
   * Every stock variant is switched OFF, so naming one on Typography,
   * ListItemText `slotProps`, DialogTitle, Link or DialogContentText (all of
   * which take `TypographyProps`) is a tsc error. `body1` stays on: MUI
   * names it internally, and bare `<Typography>` renders it (aliased to
   * `bodyMedium`, as a `<p>`). R-T13 (`elitea/no-stock-typography-variant`)
   * catches the explicit `variant="body1"` and the untyped paths.
   */
  interface TypographyPropsVariantOverrides {
    headingLarge: true;
    headingMedium: true;
    headingSmall: true;
    labelMedium: true;
    labelSmall: true;
    bodyMedium: true;
    bodySmall: true;
    subtitle: true;
    body2: false;
    caption: false;
    h1: false;
    h2: false;
    h3: false;
    h4: false;
    h5: false;
    h6: false;
    subtitle1: false;
    subtitle2: false;
    overline: false;
    button: false;
  }
}

declare module '@mui/material/IconButton' {
  /**
   * [S1] Additive-only augmentation (no existing member touched): the
   * baseline's `IconButton` `color` vocabulary
   * (`apps/elitea-ui/src/components/IconButton.jsx:17-186`) beyond MUI's
   * built-in `primary`/`secondary`. `tertiary` is the baseline's default
   * (icon-only, no filled background); `alarm` is the destructive-action
   * skin. `tertiaryCount`/`magicAssistant`/`delete` (absolute-positioned
   * badge/single-purpose skins) are deliberately not ported — no `shared/ui`
   * call site needs them, and each is a one-off layout, not a colour.
   */
  interface IconButtonPropsColorOverrides {
    tertiary: true;
    alarm: true;
  }
}

declare module '@mui/material/Checkbox' {
  /**
   * [S1] Additive-only. The baseline's `BaseCheckbox.jsx` size ladder
   * (`xs`/`small`/`medium`/`large`/`xl`) is two names wider than MUI's
   * built-in `'small' | 'medium' | 'large'`.
   */
  interface CheckboxPropsSizeOverrides {
    xs: true;
    xl: true;
  }
}
