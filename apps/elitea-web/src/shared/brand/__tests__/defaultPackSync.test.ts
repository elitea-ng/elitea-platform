import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_HUE, DEFAULT_PACK_ACCENTS, DEFAULT_PRODUCT } from '@elitea/client-core/brand';

import { DEFAULT_BRAND_PACK } from '../tokens';

/**
 * The default pack has hand-maintained copies since gen-brand-tokens.mjs went
 * with the apps/elitea-ui submodule whose palettes it read: default.pack.json,
 * palette.augment.d.ts (the typed members), client-core's identity.ts and its
 * defaultPack.gen.ts (the mobile app's accent inputs). The generator's
 * `--check` used to hold them together; this file does now.
 */

const AUGMENT_PATH = join(dirname(fileURLToPath(import.meta.url)), '../tokens/palette.augment.d.ts');
/** MUI's own colour roles. The generator folded their extra leaves into PaletteColor. */
const ROLES = new Set(['primary', 'secondary', 'info', 'warning', 'error', 'success']);
/** Leaves MUI already declares on PaletteColor. */
const BUILT_IN_ROLE_LEAVES = new Set(['main', 'light', 'dark', 'contrastText']);

/**
 * Reads `interface <Name> { ... }` blocks of the augmentation and returns each
 * interface's dotted leaf paths. Only the shapes the file uses are handled:
 * `name: {` opens a group, `name: string;` / `name: number;` is a leaf, `};`
 * or `}` closes. Anything else inside an interface fails loudly.
 */
/** One line inside an interface body: a group opens, a leaf records, a brace closes. */
function readMemberLine(line: string, path: string[], members: string[]): 'open' | 'closed' {
  const group = /^(\w+): \{$/.exec(line);
  const leaf = /^(\w+): (string|number);$/.exec(line);
  if (group) {
    path.push(group[1] ?? '');
  } else if (leaf) {
    members.push([...path, leaf[1] ?? ''].join('.'));
  } else if (line === '};' || line === '}') {
    if (path.length === 0) return 'closed';
    path.pop();
  } else if (line !== '') {
    throw new Error(`palette.augment.d.ts: unexpected line inside an interface: ${line}`);
  }
  return 'open';
}

function readAugmentation(source: string): Map<string, string[]> {
  const interfaces = new Map<string, string[]>();
  let current: string[] | null = null;
  const path: string[] = [];
  for (const raw of source.split('\n')) {
    const line = raw.trim();
    const open = /^interface (\w+) \{$/.exec(line);
    if (open) {
      current = [];
      interfaces.set(open[1] ?? '', current);
    } else if (current !== null && readMemberLine(line, path, current) === 'closed') {
      current = null;
    }
  }
  return interfaces;
}

/** The pack's token ids as the generator split them across the interfaces. */
function expectedMembers(tokenIds: string[]) {
  const palette: string[] = [];
  const roleExtras = new Set<string>();
  for (const id of tokenIds) {
    const [head = '', ...rest] = id.split('.');
    if (ROLES.has(head)) {
      if (!BUILT_IN_ROLE_LEAVES.has(rest[0] ?? '')) roleExtras.add(rest.join('.'));
    } else {
      palette.push(id);
    }
  }
  return { palette: palette.sort(), roleExtras: [...roleExtras].sort() };
}

describe('default brand pack copies agree', () => {
  it('takes its product name and hue from the client-core identity', () => {
    expect(DEFAULT_BRAND_PACK.product.name).toBe(DEFAULT_PRODUCT.name);
    expect(DEFAULT_BRAND_PACK.product.shortName).toBe(DEFAULT_PRODUCT.shortName);
    expect(DEFAULT_BRAND_PACK.brand.hue).toBe(DEFAULT_BRAND_HUE);
  });

  it('carries the same accents as client-core DEFAULT_PACK_ACCENTS', () => {
    expect(DEFAULT_PACK_ACCENTS.hue).toBe(DEFAULT_BRAND_PACK.brand.hue);
    for (const scheme of ['light', 'dark'] as const) {
      const record = DEFAULT_BRAND_PACK.schemes[scheme];
      expect(DEFAULT_PACK_ACCENTS[scheme].accent).toBe(record['primary.main']);
      expect(DEFAULT_PACK_ACCENTS[scheme].onAccent).toBe(record['text.button.primary']);
    }
  });

  it('declares both schemes with the same token ids', () => {
    const dark = Object.keys(DEFAULT_BRAND_PACK.schemes.dark).sort();
    expect(Object.keys(DEFAULT_BRAND_PACK.schemes.light).sort()).toEqual(dark);
  });

  it('types exactly the pack token ids in palette.augment.d.ts, both directions', () => {
    const interfaces = readAugmentation(readFileSync(AUGMENT_PATH, 'utf8'));
    const declared = [
      ...(interfaces.get('Palette') ?? []),
      ...(interfaces.get('TypeBackground') ?? []).map((leaf) => `background.${leaf}`),
      ...(interfaces.get('TypeText') ?? []).map((leaf) => `text.${leaf}`),
    ].sort();
    const expected = expectedMembers(Object.keys(DEFAULT_BRAND_PACK.schemes.dark));

    // A floor, so a parser that silently reads nothing cannot pass.
    expect(declared.length).toBeGreaterThan(300);
    expect(declared).toEqual(expected.palette);
    expect([...(interfaces.get('PaletteColor') ?? [])].sort()).toEqual(expected.roleExtras);
  });
});
