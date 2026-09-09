#!/usr/bin/env node
/**
 * Unit T1 — generate the DEFAULT brand pack from the canonical baseline
 * palettes (spec §4.2 tier 0; §4.5 "elitea-ui is canonical", confirmed by
 * unit T2's triage).
 *
 * Emitted (both committed; regenerate and diff, never hand-edit):
 *   src/shared/brand/tokens/default.pack.json      the pack, zod-valid
 *   src/shared/brand/tokens/palette.augment.d.ts   MUI module augmentation
 *
 * Usage (the command of record, also recorded in parity/brand-hue-map.md):
 *   node scripts/gen-brand-tokens.mjs \
 *     --baseline /path/to/elitea-platform/apps/elitea-ui
 *
 * The conversion is mechanical: darkPalette.js / lightPalette.js are
 * evaluated as data modules and flattened to `a.b.c` token ids. Everything
 * that is NOT mechanical is one of the four explicit tables below, each row
 * carrying its justification, so a reviewer can see exactly what was added to,
 * withheld from, or corrected on top of a verbatim copy.
 */
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

import {
  asymmetry,
  evalPaletteModule,
  flattenPalette,
  keyTree,
  renderTypeLiteral,
} from './lib/brand-tokens-core.mjs';
import { compareGenerated } from './lib/generated-drift.mjs';

const APP_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const OUT_DIR = join(APP_DIR, 'src/shared/brand/tokens');

/**
 * (1) EXCLUSIONS — present in a baseline file, deliberately NOT in the pack.
 * Each row cites the unit-T2 finding or the structural reason.
 */
const EXCLUSIONS = [
  {
    id: 'palette.mode',
    reason:
      'scheme identity, not a colour token — buildTheme sets palette.mode per colorScheme; MUI also skips it when generating CSS variables (shouldSkipGeneratingVar).',
  },
  {
    id: 'typographyVariants.labelLarge',
    reason:
      'T2 §3 class (b): dead in the canonical app too (0 consumers outside MainTheme.js). Not ported — see src/shared/brand/typography.ts.',
  },
  {
    id: 'background.moderator (+ const red20)',
    reason:
      'T2 §1.4/§1.16 class (b): admin-ui/Maintenance-UI only, removed from canonical in elitea-ui d46cb93 (EL-5170). Asserted ABSENT from the canonical palettes below.',
  },
  {
    id: 'icon.fill.select',
    reason:
      'T2 §3 class (b): phantom reference in admin-ui MuiRadio/MuiCheckbox; exists in no palette. Asserted ABSENT below.',
  },
];

/** Token ids that must NOT exist in the canonical baseline (T2 bug rows). */
const MUST_BE_ABSENT = ['background.moderator', 'icon.fill.select'];

/**
 * (2) SYMMETRY FILLS — the baseline's three asymmetric ids. A brand pack's
 * two scheme records must carry the same key set, otherwise a token has no
 * reference geometry to derive from in one scheme. Every fill below is
 * pixel-neutral: the id has no read site in the scheme being filled.
 */
const SYMMETRY_FILLS = [
  {
    id: 'primary.hover',
    scheme: 'light',
    value: 'rgba(244, 124, 255, 1)',
    reason:
      'dark-only in the baseline. Light value = magentaHover, already the light value of background.button.primary.hover and background.tab.hover. Only read site is BaseBtn.jsx:149 inside the dark branch, so light rendering is unchanged.',
  },
  {
    id: 'text.button.auxiliary',
    scheme: 'dark',
    value: '#83EFFF',
    reason:
      'light-only in the baseline. Dark value = primaryHover, the value BaseBtn.jsx:149 uses in the dark branch. Only read site is that same line inside the light branch, so dark rendering is unchanged.',
  },
  {
    id: 'boxShadow.aiAnswer',
    scheme: 'dark',
    value: 'none',
    reason:
      'light-only in the baseline, but read unconditionally (ApplicationAnswer.jsx:1020, UserMessage.jsx:286). In dark it currently resolves to undefined and the declaration is dropped; "none" is the exact CSS equivalent.',
  },
  {
    id: 'background.folder.shadow',
    scheme: 'dark',
    value: '0 0 0 rgba(0, 0, 0, 0)',
    reason:
      'Appeared at baseline 20b23c42, light-only, and read unconditionally as an interpolated filter argument: `filter: drop-shadow(${folder.shadow})` (entities/folder/ui/FolderItem.jsx:137,143). In dark it resolves to undefined, which makes the whole filter declaration invalid and therefore dropped, so the folder renders with no drop shadow. A transparent zero-offset shadow is the exact rendering equivalent and is valid CSS — "none" is NOT usable here, because drop-shadow() takes a shadow, not the box-shadow keyword.',
  },
  {
    id: 'background.folder.borderGradient',
    scheme: 'light',
    value: 'none',
    reason:
      'Appeared at baseline 20b23c42, dark-only, and read unconditionally as `background: folder.borderGradient` (entities/folder/ui/FolderItem.jsx:113). In light it resolves to undefined and the declaration is dropped, leaving the element with no background image; "none" is the exact CSS equivalent for a background shorthand.',
  },
];

/**
 * (3) ADDITIONS — new token ids. Two groups:
 *
 * (3a) BRAND-ACCENT tokens: the §4.1 Blocker-1 resolution. The 15
 *      `palette.mode ===` call sites in BaseBtn.jsx are two hard-coded accent
 *      ramps (dark cyan / light magenta) plus five token-pair branches. Each
 *      becomes one token id with a per-scheme value, so the branch disappears
 *      and the value is white-labelable. Values are copied verbatim from the
 *      call site, so rendering is unchanged. Full table: parity/brand-hue-map.md.
 *
 * (3b) MANDATORY ROLES: spec §4.2 — palette.error / palette.success as
 *      top-level MUI roles. Absent today; 10+ consumers silently get MUI
 *      defaults. Values are the evidenced nested tokens they replace.
 */
const ADDITIONS = [
  // (3a) special variant — BaseBtn.jsx:52,56,59 (background) / 53,60 (colour)
  ['background.button.special.default', 'rgba(106, 232, 250, 0.2)', 'rgba(245, 81, 249, 0.2)'],
  ['background.button.special.hover', 'rgba(106, 232, 250, 0.3)', 'rgba(245, 81, 249, 0.3)'],
  ['background.button.special.pressed', 'rgba(106, 232, 250, 0.1)', 'rgba(245, 81, 249, 0.1)'],
  ['text.button.specialDefault', '#6ae8fa', '#0E131D'],
  ['text.button.specialPressed', 'rgba(42, 189, 210, 1)', '#0E131D'],
  // (3a) maxi variant — BaseBtn.jsx:700,718,722,726 (background) / 701 (colour)
  ['background.button.maxi.default', 'rgba(41, 184, 245, 0.2)', 'rgba(196, 40, 221, 0.2)'],
  ['background.button.maxi.hover', 'rgba(41, 184, 245, 0.3)', 'rgba(196, 40, 221, 0.3)'],
  ['background.button.maxi.pressed', 'rgba(41, 184, 245, 0.1)', 'rgba(196, 40, 221, 0.1)'],
  ['text.button.maxiDefault', '#6ae8fa', '#777A83'],
  // (3a) the five token-pair branches
  ['text.button.secondaryPressed', '#A9B7C1', '#0E131D'],
  ['background.button.iconCounter.pressed', 'rgba(255, 255, 255, 0.10)', 'rgba(61, 68, 86, 0.2)'],
  ['text.button.auxiliaryDefault', 'rgba(42, 189, 210, 1)', 'rgba(196, 40, 221, 1)'],
  ['text.button.auxiliaryHover', '#83EFFF', 'rgba(244, 124, 255, 1)'],
  ['text.button.auxiliaryPressed', '#686C76', '#777A83'],
  // (3b) palette.error — main/light/dark from the alarm-button ramp
  ['error.main', '#D71616', '#D71616'],
  ['error.light', '#E74444', '#E74444'],
  ['error.dark', '#C51111', '#C51111'],
  ['error.contrastText', '#FFFFFF', '#FFFFFF'],
  // (3b) palette.success — main from status.published, dark from the positive button
  ['success.main', '#2BD48D', '#2AB37A'],
  ['success.dark', '#108D22', '#108D22'],
  ['success.contrastText', '#FFFFFF', '#FFFFFF'],
];

/**
 * (4) A11Y OVERRIDES — scheme-specific post-generation contrast fixes.
 *
 * The canonical baseline hard-codes `const dangerRed = '#D71616'` identically
 * in BOTH `darkPalette.js:45` and `lightPalette.js:13` for every error/danger
 * leaf below — a deliberate "the alarm colour doesn't change with scheme"
 * choice, not a porting gap (a mechanical port of either file alone
 * reproduces the same literal). Measured with the WCAG 2.1 relative-luminance
 * formula against the REAL dark-scheme surfaces these tokens render on
 * (`parity/` contrast audit, 2026-07-27): `#D71616` is 3.56:1 against
 * `background.default` (#0E131D) and 3.17:1 against `background.card.default`
 * (#181F2A) — both below the 4.5:1 AA bar for normal text. That bar applies
 * because these specific ids are actually consumed as TEXT, not only as
 * icon/border accents: `icon.fill.error` colours `MuiTextField.ts`'s
 * `.MuiFormHelperText-root.Mui-error` validation message; `status.rejected`
 * is used as literal text colour at >10 call sites in the canonical baseline
 * (`AnalyticsTools.jsx`, `AgentException.jsx`, `CanvasEditor.jsx`, …);
 * `text.error` is the dedicated text-role sibling of the same defect, fixed
 * pre-emptively (it has no consumer yet, but shipping it broken would
 * reintroduce the failure the moment one is added). Independently,
 * `background.button.alarm.default` fails 4.5:1 against `text.button.primary`
 * (#0E131D) — the label colour `MuiButton.ts`'s `alarm` variant paints on
 * top of this fill.
 *
 * Each override keeps hue (0deg) and saturation (~0.814) identical to
 * `dangerRed` and raises lightness only (0.465 -> 0.62) until the WORSE of
 * the two realistic dark surfaces (page `background.default`, card
 * `background.card.default`) clears 4.5:1 — the same kind of same-hue
 * lightness move `color.ts`'s `deriveColor` makes for its NEUTRAL band,
 * applied by hand here because this is a fixed same-purpose contrast fix,
 * not a brand-hue re-theme derivation. Light scheme is untouched: `#D71616`
 * already clears AA there (5.07:1 against `background.default` #F8FCFF).
 *
 * NOT overridden (measured, but already passing — see the same audit):
 *   - `error.main` — its only real consumer is `MuiAlert.ts`'s filled
 *     surface with `error.contrastText` (#FFFFFF) on top, 5.23:1. Lightening
 *     `error.main` to also satisfy the text bar is mathematically impossible
 *     here without dropping `error.contrastText`'s own 4.5:1 (verified by
 *     exhaustive search over the same hue/saturation ramp: no lightness value
 *     clears both simultaneously) — `error.main`/`error.light`/`error.dark`
 *     are a `PaletteColor` fill role (paired with `contrastText`), not a
 *     text role; `text.error` is the token for text contexts, and it is
 *     fixed below.
 *   - `background.button.danger` — icon-fill-only in both the baseline
 *     (`fill:`/`iconColor:`, never a literal button background despite the
 *     name) and this port (`BannerMessage.tsx`'s `iconColor`); 3.45:1
 *     against its real composited background (`background.errorBkg` over
 *     `background.default`) clears the 3:1 SC 1.4.11 non-text bar.
 *
 * Fast-follow (different literal, not `#D71616`, so a separate row):
 * `background.button.alarm.pressed` (`#C51111`) measured 3.07:1 against
 * `text.button.primary` — below 4.5:1. Fixed the same way: same hue (0deg),
 * closely matching saturation (~0.816 vs the family's ~0.814), lightness
 * raised only as far as needed (0.4196 -> 0.575) to clear 4.5:1 (lands at
 * 4.60:1) while staying strictly darker than both the already-fixed
 * `background.button.alarm.default` (l=0.6196) and `.hover` (l=0.5863,
 * itself unchanged and already passing at 4.70:1) — preserves a monotonic
 * default > hover > pressed lightness ramp instead of inverting it.
 */
const A11Y_OVERRIDES = [
  {
    id: 'icon.fill.error',
    scheme: 'dark',
    from: '#D71616',
    to: '#ED4F4F',
    reason:
      "3.56:1 vs background.default / 3.17:1 vs background.card.default, below 4.5:1. Consumed as literal FormHelperText colour by MuiTextField.ts's `.MuiFormHelperText-root.Mui-error` rule (the SecretField ErrorState story regression S1-H flagged).",
  },
  {
    id: 'text.error',
    scheme: 'dark',
    from: '#D71616',
    to: '#ED4F4F',
    reason:
      'Same source literal, same failure as icon.fill.error. No live consumer yet; fixed so a future `theme.vars.palette.text.error` text read does not reintroduce it.',
  },
  {
    id: 'status.rejected',
    scheme: 'dark',
    from: '#D71616',
    to: '#ED4F4F',
    reason:
      'Baseline (apps/elitea-ui) reads this token directly as literal text colour at >10 call sites (AnalyticsTools.jsx, AgentException.jsx, CanvasEditor.jsx, mcp/index.jsx, CredentialNotFoundValue.jsx), not only as an icon/border accent, so the 4.5:1 text bar applies.',
  },
  {
    id: 'background.button.alarm.default',
    scheme: 'dark',
    from: '#D71616',
    to: '#ED4F4F',
    reason:
      "MuiButton.ts's `alarm`/`elitea+alarm` variants paint `text.button.primary` (#0E131D) on top of this fill for the button label: 3.56:1 before, 5.16:1 after. MuiIconButton.ts's alarm variant paints a white icon instead (icon bar, 3:1): 5.23:1 before, 3.60:1 after — still clears.",
  },
  {
    id: 'background.button.alarm.pressed',
    scheme: 'dark',
    from: '#C51111',
    to: '#EB3A3A',
    reason:
      "MuiButton.ts's `alarm`/`elitea+alarm` variants paint `text.button.primary` (#0E131D) on top of this fill for the button's `:active` label: 3.07:1 before, 4.60:1 after. Same hue (0deg) as the rest of the family, lightness raised only as far as needed (0.4196 -> 0.575) to stay strictly darker than the already-fixed `.default` (l=0.6196) and unchanged `.hover` (l=0.5863, 4.70:1) — preserves a default > hover > pressed lightness ramp.",
  },
];

/** Applies (4): mutates the matching scheme record in place, id by id. */
function applyA11yOverrides(schemes) {
  for (const { id, scheme, from, to } of A11Y_OVERRIDES) {
    if (schemes[scheme][id] !== from) {
      throw new Error(
        `A11Y_OVERRIDES stale: expected ${scheme}.${id} to be ${JSON.stringify(from)} (the baseline literal this override was measured against) but found ${JSON.stringify(schemes[scheme][id])} — the baseline moved, re-derive this override's contrast math before applying it`,
      );
    }
    schemes[scheme][id] = to;
  }
}

/**
 * The default pack's non-colour fields. `brand.hue` is the single
 * scheme-independent hue (spec §4.2); at the default pack every token is
 * stated verbatim, so the hue's derivation is fully shadowed and the value
 * is a pure data decision — see the HUMAN DECISION section of
 * parity/brand-hue-map.md.
 */
const PACK_META = {
  $schema: 'https://elitea.ai/schemas/brand-pack/1.json',
  id: 'default',
  version: '1.0.0',
  // docsUrl: the same-origin embedded docs SPA (issue W1b). Relative and
  // root-relative so every deployment resolves it against its own origin
  // with no rebuild; an admin brand-pack override may still set an absolute
  // external URL instead (see schema.ts's relativeOrAbsoluteUrl).
  product: { name: 'Elitea', shortName: 'Elitea', docsUrl: '/docs/' },
  assets: {
    logoFull: './brand/logo-full.svg',
    logoMark: './brand/logo-mark.svg',
    favicon: './brand/favicon.svg',
  },
  typography: {
    // MainTheme.js:113 verbatim; baseSize/scale reproduce the baseline's five
    // distinct variant sizes exactly (see src/shared/brand/typography.ts).
    fontFamily: '"Montserrat", Roboto, Arial, sans-serif',
    fontFamilyMono: '"Roboto Mono", Consolas, "Courier New", monospace',
    baseSize: 14,
    scale: 1.2,
  },
  // radiusMd 8 == the 0.5rem used by MuiMenu/MuiAutocomplete; radiusLg 16 ==
  // MuiDialog's 1rem; radiusSm 4 == the 0.25rem of the small controls.
  // radiusPill 9999 == unit S1 (Part B, MuiButton OWNERSHIP.md note 2): the
  // baseline's icon-only button / `maxi` FAB use `borderRadius: '50%'`,
  // which `elitea/ad-hoc-radius` rejects (no token member-expression form
  // for a literal shape) and which none of radiusSm|Md|Lg represent (a
  // pill/circle, not a corner rounding rung). A large fixed px value clamps
  // to the shorter box dimension in CSS, so it renders as a true pill on a
  // rectangular box and a true circle on a square one, regardless of size.
  shape: { radiusSm: 4, radiusMd: 8, radiusLg: 16, radiusPill: 9999, density: 'comfortable' },
  locale: { default: 'en-GB', dateLocale: 'en-GB' },
  brand: { hue: '#6ae8fa' },
};

function parseArgs(argv) {
  const args = { baseline: null, check: false };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--baseline') {
      const value = argv[i + 1];
      if (!value) throw new Error('--baseline requires a path to apps/elitea-ui');
      args.baseline = resolve(value);
      i++;
    } else if (argv[i] === '--check') {
      args.check = true;
    } else {
      throw new Error(`unknown argument: ${argv[i]}`);
    }
  }
  if (!args.baseline) throw new Error('--baseline <path to apps/elitea-ui> is required');
  return args;
}

function loadSchemes(baseline) {
  const src = join(baseline, 'src');
  const dark = evalPaletteModule(readFileSync(join(src, 'darkPalette.js'), 'utf8'));
  const light = evalPaletteModule(readFileSync(join(src, 'lightPalette.js'), 'utf8'), {
    white: dark.consts.white,
  });
  return {
    dark: flattenPalette(dark.palette, ['mode']),
    light: flattenPalette(light.palette, ['mode']),
  };
}

function assertBugTokensAbsent(schemes) {
  for (const id of MUST_BE_ABSENT) {
    for (const scheme of ['light', 'dark']) {
      if (id in schemes[scheme]) {
        throw new Error(`T2 bug token ${id} unexpectedly present in the canonical ${scheme} palette`);
      }
    }
  }
}

function applySymmetryFills(schemes) {
  const { lightOnly, darkOnly } = asymmetry(schemes.light, schemes.dark);
  const expected = new Set(SYMMETRY_FILLS.map((f) => f.id));
  for (const id of [...lightOnly, ...darkOnly]) {
    if (!expected.has(id)) throw new Error(`unhandled asymmetric token id: ${id}`);
  }
  for (const fill of SYMMETRY_FILLS) {
    if (fill.id in schemes[fill.scheme]) {
      throw new Error(`symmetry fill ${fill.id} is already present in the ${fill.scheme} scheme`);
    }
    // A fill supplies the MISSING half of a token the baseline defines in the
    // other scheme. If the other scheme has not got it either, the fill is
    // stale — the baseline dropped the token, or the pin moved backwards — and
    // applying it would invent a token no baseline has. Say so here rather
    // than letting it surface later as "schemes still asymmetric", which names
    // the symptom and not the stale table entry.
    const other = fill.scheme === 'light' ? 'dark' : 'light';
    if (!(fill.id in schemes[other])) {
      throw new Error(
        `stale symmetry fill ${fill.id}: absent from BOTH schemes in this baseline, so there is no asymmetry to fill`,
      );
    }
    schemes[fill.scheme][fill.id] = fill.value;
  }
}

function applyAdditions(schemes) {
  for (const [id, darkValue, lightValue] of ADDITIONS) {
    if (id in schemes.dark || id in schemes.light) {
      throw new Error(`addition ${id} already exists in the baseline — it is not an addition`);
    }
    schemes.dark[id] = darkValue;
    schemes.light[id] = lightValue;
  }
}

function applyTables(schemes) {
  assertBugTokensAbsent(schemes);
  applySymmetryFills(schemes);
  applyAdditions(schemes);
  applyA11yOverrides(schemes);
  const post = asymmetry(schemes.light, schemes.dark);
  if (post.lightOnly.length || post.darkOnly.length) {
    throw new Error(`schemes still asymmetric: ${JSON.stringify(post)}`);
  }
  return schemes;
}

/** Deterministic output: token ids sorted, so a regeneration diff is minimal. */
function sortRecord(record) {
  return Object.fromEntries(Object.keys(record).sort().map((k) => [k, record[k]]));
}

/**
 * MUI type augmentation. Groups that collide with a built-in MUI palette
 * member are folded into that member's own augmentable interface instead of
 * Palette: background -> TypeBackground, text -> TypeText, and the six colour
 * roles' extra leaves -> PaletteColor.
 */
function renderAugmentation(tokenIds) {
  const tree = keyTree(tokenIds);
  const roleExtras = {};
  for (const role of ['primary', 'secondary', 'info', 'warning', 'error', 'success']) {
    const node = tree[role];
    if (!node || node === true) continue;
    for (const [leaf, value] of Object.entries(node)) {
      if (['main', 'light', 'dark', 'contrastText'].includes(leaf)) continue;
      roleExtras[leaf] = value;
    }
    delete tree[role];
  }
  const background = tree.background;
  const text = tree.text;
  delete tree.background;
  delete tree.text;

  const groups = Object.entries(tree)
    .map(([name, node]) => `    ${name}: ${renderTypeLiteral(node, 6)};`)
    .join('\n');

  return `/* oxlint-disable max-lines --
   GENERATED file: one member per token id, so its length is the token count,
   not a complexity signal. scripts/check-budgets.mjs exempts .d.ts from the
   §3.5 file-length budget for the same reason; oxlint's max-lines has no
   generated-file notion, so the exemption is declared here, in the generator. */
/**
 * GENERATED by scripts/gen-brand-tokens.mjs — do not edit by hand.
 *
 * Module augmentation for the Elitea token set (spec §4.2). Every id in the
 * default pack's scheme records becomes a typed member, so component code
 * reads \`theme.vars.palette.<path>\` (R-T7) with full type checking; MUI
 * derives \`theme.vars.palette\` from \`Palette\`, so augmenting \`Palette\`
 * (and the two structural interfaces it delegates to) is sufficient.
 *
 * Members are declared on the READ side only (Palette / TypeBackground /
 * TypeText / PaletteColor). The *Options* side stays MUI's, because
 * toMuiPalette builds its result dynamically and hands it over as
 * PaletteOptions at one documented boundary.
 */
import '@mui/material/styles';

declare module '@mui/material/styles' {
  interface Palette {
${groups}
  }

  interface TypeBackground ${renderTypeLiteral(background, 4)}

  interface TypeText ${renderTypeLiteral(text, 4)}

  interface PaletteColor ${renderTypeLiteral(roleExtras, 4)}

  interface Shape {
    radiusSm: number;
    radiusMd: number;
    radiusLg: number;
    radiusPill: number;
  }
}
`;
}

function readOrNull(filePath) {
  try {
    return readFileSync(filePath, 'utf8');
  } catch {
    return null;
  }
}

/**
 * --check mode (issue #490): compare both committed outputs with what this
 * run renders, and write nothing. The header of this file already states the
 * rule ("both committed; regenerate and diff, never hand-edit"); this is what
 * performs the diff. An absent committed file and an empty render are both
 * failures — see scripts/lib/generated-drift.mjs.
 */
function runCheck(packJson, augmentTs) {
  const result = compareGenerated([
    {
      path: 'apps/elitea-web/src/shared/brand/tokens/default.pack.json',
      expected: packJson,
      actual: readOrNull(join(OUT_DIR, 'default.pack.json')),
    },
    {
      path: 'apps/elitea-web/src/shared/brand/tokens/palette.augment.d.ts',
      expected: augmentTs,
      actual: readOrNull(join(OUT_DIR, 'palette.augment.d.ts')),
    },
  ]);
  if (!result.ok) {
    console.error('gen-brand-tokens: --check FAIL');
    for (const failure of result.failures) console.error(`  ${failure}`);
    console.error('  command: node scripts/gen-brand-tokens.mjs --baseline ../elitea-ui');
    process.exit(1);
  }
  console.log('gen-brand-tokens: --check OK — default.pack.json and palette.augment.d.ts are up to date.');
}

function main() {
  const { baseline, check } = parseArgs(process.argv.slice(2));
  const schemes = applyTables(loadSchemes(baseline));
  const pack = {
    ...PACK_META,
    schemes: { light: sortRecord(schemes.light), dark: sortRecord(schemes.dark) },
  };
  const packJson = `${JSON.stringify(pack, null, 2)}\n`;
  const augmentTs = renderAugmentation(Object.keys(pack.schemes.dark));

  if (check) {
    runCheck(packJson, augmentTs);
    return;
  }

  mkdirSync(OUT_DIR, { recursive: true });
  writeFileSync(join(OUT_DIR, 'default.pack.json'), packJson, 'utf8');
  writeFileSync(join(OUT_DIR, 'palette.augment.d.ts'), augmentTs, 'utf8');

  const n = Object.keys(pack.schemes.dark).length;
  console.log(`gen-brand-tokens: ${n} token ids per scheme (baseline + fills + additions)`);
  console.log(
    `  exclusions: ${EXCLUSIONS.length}, symmetry fills: ${SYMMETRY_FILLS.length}, additions: ${ADDITIONS.length}, a11y overrides: ${A11Y_OVERRIDES.length}`,
  );
  console.log(`  wrote ${join(OUT_DIR, 'default.pack.json')}`);
  console.log(`  wrote ${join(OUT_DIR, 'palette.augment.d.ts')}`);
}

main();
