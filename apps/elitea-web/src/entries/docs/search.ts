/**
 * Client-side search over the docs (PREAMBLE decision 2). The index itself
 * is built at build time by `scripts/docs-build-search-index.mjs`, which
 * walks `content/**\/*.mdx`, strips MDX/JSX syntax down to prose, and writes
 * `search-index.generated.json` (committed, like `src/routeTree.gen.ts` —
 * see that script's header for why).
 *
 * Both the `minisearch` package and the generated JSON are loaded LAZILY,
 * via a dynamic `import()` inside `getIndex()`, not at this module's top
 * level. The JSON alone is ~85 KiB gzip (126 real pages' full prose, not the
 * handful of placeholder pages this entry shipped with originally) — large
 * enough on its own to blow the docs entry's initial-bundle budget
 * (`bundle-budget.json`) if it loaded before first paint. Nothing on the
 * page needs search results before the reader actually types a query, so
 * `App.tsx`'s `SearchBox` calls the async `search()` below instead of
 * importing this module's data eagerly; the fetch for this chunk starts on
 * the first keystroke (or earlier, if a future change wants to prefetch on
 * focus) rather than being part of the entry's own JS chunk.
 */
import type MiniSearch from 'minisearch';

interface SearchDocument {
  readonly id: string;
  readonly slug: string;
  readonly title: string;
  readonly text: string;
}

export interface SearchResult {
  readonly slug: string;
  readonly title: string;
  readonly excerpt: string;
  readonly score: number;
}

let indexPromise: Promise<MiniSearch<SearchDocument>> | undefined;

async function loadIndex(): Promise<MiniSearch<SearchDocument>> {
  const [{ default: MiniSearchCtor }, { default: generatedEntries }] = await Promise.all([
    import('minisearch'),
    import('./search-index.generated.json'),
  ]);
  const miniSearch = new MiniSearchCtor<SearchDocument>({
    fields: ['title', 'text'],
    storeFields: ['slug', 'title', 'text'],
    searchOptions: { prefix: true, fuzzy: 0.2, boost: { title: 2 } },
  });
  miniSearch.addAll(generatedEntries);
  return miniSearch;
}

function getIndex(): Promise<MiniSearch<SearchDocument>> {
  indexPromise ??= loadIndex();
  return indexPromise;
}

const EXCERPT_RADIUS = 60;

function excerptAround(text: string, query: string): string {
  const lower = text.toLowerCase();
  const at = lower.indexOf(query.toLowerCase());
  if (at === -1) return text.slice(0, EXCERPT_RADIUS * 2);
  const start = Math.max(0, at - EXCERPT_RADIUS);
  const end = Math.min(text.length, at + query.length + EXCERPT_RADIUS);
  return `${start > 0 ? '…' : ''}${text.slice(start, end)}${end < text.length ? '…' : ''}`;
}

/** Searches the built-time index; empty query returns no results rather than
 * every page, so an empty search box does not paint the whole nav twice (and
 * does not itself trigger loading the index chunk). */
export async function search(query: string): Promise<SearchResult[]> {
  const trimmed = query.trim();
  if (trimmed === '') return [];
  const index = await getIndex();
  const hits = index.search(trimmed);
  return hits.map((hit) => {
    const doc = hit as unknown as SearchDocument & { score: number };
    return {
      slug: doc.slug,
      title: doc.title,
      excerpt: excerptAround(doc.text, trimmed),
      score: doc.score,
    };
  });
}
