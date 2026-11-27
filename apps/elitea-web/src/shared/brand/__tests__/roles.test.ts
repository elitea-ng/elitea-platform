import { describe, expect, it } from 'vitest';

import { BRAND_TOKENS } from '@elitea/client-core/brand';

import { buildEliteaTheme } from '../buildTheme';
import { formatHex, hslaToRgba } from '../color';
import type { BrandInput, SchemeRecord } from '../schema';
import { DEFAULT_BRAND_PACK, LEGACY_ROLES, ROLE_GROUP } from '../tokens';
import { resolveScheme } from '../toMuiPalette';

/**
 * ADR-0062 decision 1 and Verification → Roles: the 27 brand-guide roles are
 * ids `role.<name>` of the pack, equal to `tokens.json` in both schemes in the
 * default pack; a hand-tuned pack keeps its colours in the roles through
 * `tokens/legacy-roles.json` with a fixed precedence (decision 1.4); a role
 * follows the brand hue like every legacy id (decision 1.6).
 */

const SCHEMES = ['light', 'dark'] as const;
const brand = DEFAULT_BRAND_PACK.brand;
/** A colour no default id carries, the way toMuiPalette.test.ts builds one. */
const colour = (h: number) => formatHex(hslaToRgba({ h, s: 0.5, l: 0.4, a: 1 }));
const TUNED = colour(17);
const OTHER = colour(211);
/** A brand hue other than the default pack's (Base Indigo, ~239°). */
const HUE_ONLY: BrandInput = { hue: formatHex(hslaToRgba({ h: 90, s: 0.7, l: 0.5, a: 1 })) };

const roleId = (role: string) => `${ROLE_GROUP}.${role}`;
const brandRoles = (scheme: (typeof SCHEMES)[number]) => BRAND_TOKENS.schemes[scheme] as Record<string, string>;
const MAPPED = Object.entries(LEGACY_ROLES).flatMap(([role, ids]) => ids.map((id) => [role, id] as const));

describe('role ids in the default pack', () => {
  it('states the 27 roles of tokens.json, with its values, in both schemes', () => {
    for (const scheme of SCHEMES) {
      const roles = brandRoles(scheme);
      expect(Object.keys(roles)).toHaveLength(27);
      const record = DEFAULT_BRAND_PACK.schemes[scheme];
      const packRoles = Object.keys(record).filter((id) => id.startsWith(`${ROLE_GROUP}.`));
      expect(packRoles.sort()).toEqual(Object.keys(roles).map(roleId).sort());
      for (const [role, value] of Object.entries(roles)) {
        expect(record[roleId(role)], `${scheme}.${role}`).toBe(value);
      }
    }
  });

  it('resolves every role on the theme in both schemes', () => {
    const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
    for (const scheme of SCHEMES) {
      const palette = theme.colorSchemes[scheme]?.palette as unknown as Record<string, unknown>;
      expect(palette[ROLE_GROUP]).toEqual(brandRoles(scheme));
    }
    expect(theme.vars.palette.role.textMuted).toBe(
      `var(--el-palette-role-textMuted, ${BRAND_TOKENS.schemes.dark.textMuted})`,
    );
  });
});

describe('legacy-roles.json, the map rule (decision 1.4, Risks)', () => {
  it('lists brand-guide roles, each with exactly one id', () => {
    // A role cannot hold two tuned colours, so a second id would lose one.
    for (const [role, ids] of Object.entries(LEGACY_ROLES)) {
      expect(brandRoles('dark')[role], `${role} is a brand-guide role`).toBeDefined();
      expect(ids, role).toHaveLength(1);
    }
  });

  it('lists ids that exist in the pack and are not role ids', () => {
    for (const [, id] of MAPPED) {
      expect(id.startsWith(`${ROLE_GROUP}.`), id).toBe(false);
      for (const scheme of SCHEMES) expect(DEFAULT_BRAND_PACK.schemes[scheme][id], `${scheme}.${id}`).toBeDefined();
    }
  });

  it('puts each id on one list only', () => {
    const ids = MAPPED.map(([, id]) => id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it('gives each id its role’s default colour in both schemes', () => {
    for (const scheme of SCHEMES) {
      const record = DEFAULT_BRAND_PACK.schemes[scheme];
      for (const [role, id] of MAPPED) {
        expect(record[id], `${scheme}.${id} → ${role}`).toBe(record[roleId(role)]);
      }
    }
  });
});

describe('a hand-tuned pack keeps its colours in the roles (decision 1.4)', () => {
  it.each(MAPPED)('%s takes %s when a tuned deployment states only that id', (role, id) => {
    for (const scheme of SCHEMES) {
      const resolved = resolveScheme({ [id]: TUNED }, brand, scheme);
      expect(resolved[roleId(role)], scheme).toBe(TUNED);
      expect(resolved[id], scheme).toBe(TUNED);
    }
  });

  it.each(MAPPED)(
    '%s takes %s when the file layer states the product default with only that id changed',
    (role, id) => {
      for (const scheme of SCHEMES) {
        const record: SchemeRecord = { ...DEFAULT_BRAND_PACK.schemes[scheme], [id]: TUNED };
        expect(resolveScheme(record, brand, scheme)[roleId(role)], scheme).toBe(TUNED);
      }
    },
  );

  it.each(MAPPED)('%s takes %s when a tuned deployment states only that id under another hue', (role, id) => {
    for (const scheme of SCHEMES) {
      expect(resolveScheme({ [id]: TUNED }, HUE_ONLY, scheme)[roleId(role)], scheme).toBe(TUNED);
    }
  });

  it('keeps a role the pack states with a non-default value over a tuned legacy id', () => {
    for (const scheme of SCHEMES) {
      for (const [role, id] of MAPPED) {
        const resolved = resolveScheme({ [id]: TUNED, [roleId(role)]: OTHER }, brand, scheme);
        expect(resolved[roleId(role)], `${scheme}.${role}`).toBe(OTHER);
      }
    }
  });

  it('treats a role stated at its default in another hex case as the default', () => {
    const record: SchemeRecord = {
      'text.secondary': TUNED,
      'role.text': (DEFAULT_BRAND_PACK.schemes.dark['role.text'] as string).toLowerCase(),
    };
    expect(resolveScheme(record, brand, 'dark')['role.text']).toBe(TUNED);
  });

  it('keeps a legacy id pinned at its default under another hue in the role', () => {
    // Under another hue an unstated id re-hues, so stating its default is a
    // pin: the role keeps the pinned colour, not the hue-only one.
    for (const scheme of SCHEMES) {
      const hueOnly = resolveScheme({}, HUE_ONLY, scheme);
      for (const [role, id] of MAPPED) {
        const atDefault = DEFAULT_BRAND_PACK.schemes[scheme][id] as string;
        const resolved = resolveScheme({ [id]: atDefault }, HUE_ONLY, scheme);
        expect(resolved[roleId(role)], `${scheme}.${id}`).toBe(atDefault);
        expect(resolved[roleId(role)], `${scheme}.${id}`).not.toBe(hueOnly[roleId(role)]);
      }
    }
  });

  it('gives each mapped role the colour of its id when a file layer of the 408-id default sets another hue', () => {
    // A file layer written before the role ids states every legacy id at its
    // default: each is a pin under another hue, and the role follows it.
    for (const scheme of SCHEMES) {
      const before = Object.entries(DEFAULT_BRAND_PACK.schemes[scheme]).filter(
        ([id]) => !id.startsWith(`${ROLE_GROUP}.`),
      );
      const resolved = resolveScheme(Object.fromEntries(before), HUE_ONLY, scheme);
      for (const [role, id] of MAPPED) {
        expect(resolved[roleId(role)], `${scheme}.${role}`).toBe(resolved[id]);
      }
    }
  });

  it('resolves an export of the product default to tokens.json', () => {
    for (const scheme of SCHEMES) {
      const resolved = resolveScheme({ ...DEFAULT_BRAND_PACK.schemes[scheme] }, brand, scheme);
      for (const [role, value] of Object.entries(brandRoles(scheme))) {
        expect(resolved[roleId(role)], `${scheme}.${role}`).toBe(value);
      }
      expect(resolved).toEqual(DEFAULT_BRAND_PACK.schemes[scheme]);
    }
  });

  it('still paints a hand-tuned component id on its own component, and leaves the roles alone', () => {
    const resolved = resolveScheme({ 'background.tab.hover': TUNED }, brand, 'light');
    expect(resolved['background.tab.hover']).toBe(TUNED);
    const roles = Object.entries(resolved).filter(([id]) => id.startsWith(`${ROLE_GROUP}.`));
    expect(roles.every(([, value]) => value !== TUNED)).toBe(true);
  });

  it('derives a role with no legacy id as before', () => {
    const resolved = resolveScheme({ 'text.primary': TUNED }, brand, 'dark');
    expect(resolved['role.surface']).toBe(DEFAULT_BRAND_PACK.schemes.dark['role.surface']);
  });

  it('builds a theme whose role carries the tuned colour', () => {
    const pack = {
      ...DEFAULT_BRAND_PACK,
      schemes: { light: { 'text.primary': TUNED }, dark: { 'text.primary': TUNED } },
    };
    const theme = buildEliteaTheme(pack);
    for (const scheme of SCHEMES) {
      const palette = theme.colorSchemes[scheme]?.palette as unknown as Record<string, unknown>;
      expect((palette[ROLE_GROUP] as Record<string, string>)['textMuted']).toBe(TUNED);
    }
  });
});

describe('a role follows the brand hue (decision 1.6)', () => {
  it('re-hues role.primary from its default: off tokens.json, equal to the resolved primary.main', () => {
    for (const scheme of SCHEMES) {
      const resolved = resolveScheme({}, HUE_ONLY, scheme);
      expect(resolved['role.primary'], scheme).not.toBe(brandRoles(scheme)['primary']);
      expect(resolved['role.primary'], scheme).toBe(resolved['primary.main']);
    }
  });

  it('gives every role on the map the colour of its legacy id', () => {
    for (const scheme of SCHEMES) {
      const resolved = resolveScheme({}, HUE_ONLY, scheme);
      for (const [role, ids] of Object.entries(LEGACY_ROLES)) {
        expect(resolved[roleId(role)], `${scheme}.${role}`).toBe(resolved[ids[0] as string]);
      }
    }
  });

  it('re-hues every role from its own default value', () => {
    // The same rehue as every legacy id; at the default hue it is the identity.
    for (const scheme of SCHEMES) {
      const atDefaultHue = resolveScheme({}, brand, scheme);
      for (const [role, value] of Object.entries(brandRoles(scheme))) {
        expect(atDefaultHue[roleId(role)], `${scheme}.${role}`).toBe(value);
      }
    }
  });
});

describe('brand.onBrand reaches the labels of brand-coloured roles', () => {
  /** A light brand hue, where the default white label fails the contrast of tokens.json. */
  const LIGHT_HUE: BrandInput = { hue: '#FFD400', onBrand: '#0B0B18' };

  it('gives role.onPrimary the onBrand value, as primary.contrastText gets it', () => {
    const theme = buildEliteaTheme({ ...DEFAULT_BRAND_PACK, brand: LIGHT_HUE, schemes: { light: {}, dark: {} } });
    for (const scheme of SCHEMES) {
      expect(resolveScheme({}, LIGHT_HUE, scheme)['role.onPrimary'], scheme).toBe('#0B0B18');
      const palette = theme.colorSchemes[scheme]?.palette as unknown as Record<string, Record<string, string>>;
      expect(palette[ROLE_GROUP]?.['onPrimary'], scheme).toBe(palette['primary']?.['contrastText']);
    }
  });

  it('keeps a tuned role.onPrimary over onBrand', () => {
    for (const scheme of SCHEMES) {
      expect(resolveScheme({ 'role.onPrimary': TUNED }, LIGHT_HUE, scheme)['role.onPrimary'], scheme).toBe(TUNED);
    }
  });

  it('gives role.onSignal the onBrand value only where Signal has the anchor colour (light)', () => {
    expect(resolveScheme({}, LIGHT_HUE, 'light')['role.onSignal']).toBe('#0B0B18');
    expect(resolveScheme({ 'role.onSignal': TUNED }, LIGHT_HUE, 'light')['role.onSignal']).toBe(TUNED);
    const darkHueOnly = resolveScheme({}, { hue: LIGHT_HUE.hue }, 'dark')['role.onSignal'];
    expect(resolveScheme({}, LIGHT_HUE, 'dark')['role.onSignal']).toBe(darkHueOnly);
    for (const scheme of SCHEMES) {
      expect(resolveScheme({}, LIGHT_HUE, scheme)['role.onDangerFill'], scheme).toBe(
        resolveScheme({}, { hue: LIGHT_HUE.hue }, scheme)['role.onDangerFill'],
      );
    }
  });

  it('leaves role.onPrimary alone when the pack has no onBrand', () => {
    for (const scheme of SCHEMES) {
      const atDefaultHue = resolveScheme({}, brand, scheme)['role.onPrimary'];
      expect(atDefaultHue, scheme).toBe(brandRoles(scheme)['onPrimary']);
    }
  });
});
