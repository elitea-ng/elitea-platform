/**
 * Loads every `content/**\/*.mdx` page eagerly (`import.meta.glob`, `eager:
 * true`) and indexes it by slug. `App.tsx` renders off this registry.
 * `docs-content.test.ts` does NOT import it — `vitest.config.ts` registers
 * no MDX plugin (see that test file's own header), so it re-derives the same
 * slug list from the raw files on disk instead. `slugFromPath` here and
 * `slugFor` there must therefore agree, and both files say so.
 *
 * `eager: true` (rather than the lazy, per-route dynamic `import()` a bigger
 * docs site would want) is deliberate for a handful of placeholder pages:
 * every page ships in the entry's one JS chunk, and `router.ts` never has to
 * juggle a loading state for "which page is this slug" — only the images
 * `Screenshot.tsx` glob-imports are looked up lazily-by-URL, not the pages
 * themselves.
 */
import type { ComponentType } from 'react';

import type { MdxFrontmatter } from './content-types';

interface MdxModule {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- the
  // component map's own values are `ComponentType<any>` for the same
  // contravariance reason `components/index.ts` documents.
  readonly default: ComponentType<{ components?: Record<string, ComponentType<any>> }>;
  readonly frontmatter: MdxFrontmatter;
}

const modules = import.meta.glob<MdxModule>('./content/**/*.mdx', { eager: true });

export interface ContentEntry {
  readonly slug: string;
  readonly frontmatter: MdxFrontmatter;
  readonly Component: MdxModule['default'];
}

function slugFromPath(path: string): string {
  // `path` looks like './content/index.mdx' or './content/quick-start.mdx'.
  const rel = path.replace(/^\.\/content\//, '').replace(/\.mdx$/, '');
  return rel === 'index' ? '' : rel;
}

const entries = new Map<string, ContentEntry>();
for (const [path, mod] of Object.entries(modules)) {
  const slug = slugFromPath(path);
  entries.set(slug, { slug, frontmatter: mod.frontmatter, Component: mod.default });
}

export function getContentEntry(slug: string): ContentEntry | undefined {
  return entries.get(slug);
}
