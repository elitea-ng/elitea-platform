/**
 * Build-time only: `vite.config.ts` runs this over `en.json` before the
 * catalogue is bundled. Nothing in the running app imports it.
 *
 * The flat catalogue repeats every dotted prefix once per key
 * (`features.chatInput.voice.` appears in front of each voice string). The
 * whole catalogue ships in the initial set of both the app and the admin
 * entry, so those repeats are initial-bundle bytes. Folding the keys into a
 * tree on their dots writes each prefix once: measured 2026-10-05 on the
 * 4,590-pair catalogue, 68.2 KiB gzip sorted-flat against 59.6 KiB nested.
 *
 * i18next reads both shapes with the same `t('a.b.c')` call: its default
 * `keySeparator` is `.`, so it walks the tree first and, when that finds no
 * string, falls back to a flat lookup of the whole key (`ignoreJSONStructure`,
 * on by default). That fallback is what keeps the keys that cannot nest
 * working: when `a.b` is a string, `a.b.c` has no object to live in, so it
 * stays a flat key at the root. `nestCatalogue.test.ts` checks every key in
 * `en.json` resolves to its own string through a real i18next instance.
 */
export type CatalogueTree = { [segment: string]: string | CatalogueTree };

const hasOwn = (node: CatalogueTree, segment: string): boolean => Object.prototype.hasOwnProperty.call(node, segment);

/** Folds flat dotted keys into a tree; a key that cannot nest stays flat at the root. Keys are sorted first, so a prefix key always lands before its extensions. */
export function nestCatalogue(flat: Readonly<Record<string, string>>): CatalogueTree {
  const root: CatalogueTree = {};
  const keys = Object.keys(flat).sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
  for (const key of keys) {
    const value = flat[key] as string;
    const segments = key.split('.');
    const leaf = segments.pop() as string;
    // An empty segment (`a..b`, a leading or trailing dot) has no tree path that
    // i18next would walk back to this key, so it stays flat.
    let node: CatalogueTree | undefined = segments.includes('') || leaf === '' ? undefined : root;
    for (const segment of segments) {
      if (node === undefined) break;
      if (!hasOwn(node, segment)) Object.defineProperty(node, segment, { value: {}, enumerable: true, writable: true });
      const next: string | CatalogueTree | undefined = node[segment];
      node = typeof next === 'object' ? next : undefined;
    }
    if (node !== undefined) {
      Object.defineProperty(node, leaf, { value, enumerable: true, writable: true });
    } else {
      Object.defineProperty(root, key, { value, enumerable: true, writable: true });
    }
  }
  return root;
}
