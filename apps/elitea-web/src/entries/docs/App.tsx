/**
 * The docs SPA shell (PREAMBLE decision 2): top bar (product name, search,
 * theme toggle), left nav (tabs → groups → pages), a content column, a
 * right-rail "On this page" TOC built from the rendered page's own
 * headings, and a prev/next footer.
 *
 * No routing library and no MUI (decision 1) — `router.ts` is the whole
 * routing surface, and every visual piece here is plain CSS
 * (`styles.css`).
 */
import { Suspense, useCallback, useEffect, useRef, useState } from 'react';
import type { RefObject } from 'react';

import { t } from '@/shared/i18n';

import { docsComponents } from './components';
import { getContentEntry } from './content-registry';
import { nav, neighbours, type NavGroup, type NavTab } from './nav';
import { currentSlug, handleDocsLinkClick, initHistoryListener, onRouteChange, toPath } from './router';
import { search, type SearchResult } from './search';

const SITE_NAME = 'Elitea Docs';
const DEFAULT_DESCRIPTION = 'Elitea documentation: guides and reference for the Elitea platform.';

type Theme = 'system' | 'light' | 'dark';
const THEME_STORAGE_KEY = 'docs-theme';

function readStoredTheme(): Theme {
  try {
    const stored = window.localStorage.getItem(THEME_STORAGE_KEY);
    if (stored === 'light' || stored === 'dark') return stored;
  } catch {
    // Storage unavailable (private browsing, disabled cookies): fall back.
  }
  return 'system';
}

function applyTheme(theme: Theme): void {
  if (theme === 'system') {
    document.documentElement.removeAttribute('data-docs-theme');
  } else {
    document.documentElement.setAttribute('data-docs-theme', theme);
  }
  try {
    if (theme === 'system') window.localStorage.removeItem(THEME_STORAGE_KEY);
    else window.localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    // Best-effort persistence only.
  }
  window.dispatchEvent(new Event('docs:theme-change'));
}

function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>(readStoredTheme);

  const cycle = (): void => {
    const next: Theme = theme === 'system' ? 'light' : theme === 'light' ? 'dark' : 'system';
    setTheme(next);
    applyTheme(next);
  };

  const label = theme === 'system' ? 'Theme: system' : theme === 'light' ? 'Theme: light' : 'Theme: dark';

  return (
    <button
      type="button"
      className="docs-theme-toggle"
      onClick={cycle}
      aria-label={t('entries.docs.themeToggle.ariaLabel', 'Toggle color theme')}
    >
      {label}
    </button>
  );
}

function SearchBox() {
  const [query, setQuery] = useState('');
  const [results, setResults] = useState<SearchResult[]>([]);
  // `search()` lazily loads its index (`search.ts`'s header explains why:
  // ~85 KiB gzip of prose, too heavy for the initial bundle) and resolves
  // asynchronously, so a fast typist can have an older query's results
  // arrive after a newer one's. `requestIdRef` discards any response that
  // isn't for the latest keystroke.
  const requestIdRef = useRef(0);

  return (
    <div className="docs-topbar__search">
      <input
        type="search"
        placeholder={t('entries.docs.search.placeholder', 'Search docs…')}
        value={query}
        onChange={(event) => {
          const value = event.target.value;
          setQuery(value);
          const requestId = ++requestIdRef.current;
          void search(value).then((newResults) => {
            if (requestIdRef.current === requestId) setResults(newResults);
          });
        }}
      />
      {results.length > 0 ? (
        <ul className="docs-search-results">
          {results.map((result) => (
            <li key={result.slug}>
              <a
                href={toPath(result.slug)}
                onClick={() => {
                  setQuery('');
                  setResults([]);
                }}
              >
                <span className="docs-search-results__title">{result.title}</span>
                <span className="docs-search-results__excerpt">{result.excerpt}</span>
              </a>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

function NavGroupView({ group, activeSlug }: { readonly group: NavGroup; readonly activeSlug: string | undefined }) {
  return (
    <div className="docs-nav__group">
      <p className="docs-nav__group-title">{group.title}</p>
      <ul className="docs-nav__pages">
        {group.pages.map((page) => (
          <li key={page.slug}>
            <NavLink slug={page.slug} title={page.title} active={page.slug === activeSlug} />
          </li>
        ))}
      </ul>
      {(group.groups ?? []).map((child) => (
        <NavGroupView key={child.title} group={child} activeSlug={activeSlug} />
      ))}
    </div>
  );
}

function NavLink({ slug, title, active }: { readonly slug: string; readonly title: string; readonly active: boolean }) {
  return (
    <a href={toPath(slug)} className={`docs-nav__link${active ? ' docs-nav__link--active' : ''}`}>
      {title}
    </a>
  );
}

function NavView({ activeSlug }: { readonly activeSlug: string | undefined }) {
  return (
    <nav className="docs-nav" aria-label={t('entries.docs.nav.ariaLabel', 'Documentation')}>
      {nav.map((tab: NavTab) => (
        <div className="docs-nav__tab" key={tab.title}>
          <p className="docs-nav__tab-title">{tab.title}</p>
          {tab.groups.map((group) => (
            <NavGroupView key={group.title} group={group} activeSlug={activeSlug} />
          ))}
        </div>
      ))}
    </nav>
  );
}

interface TocItem {
  readonly id: string;
  readonly text: string;
}

/** Reads "On this page" straight off the rendered DOM (`h2`/`h3` inside the
 * content column) instead of re-parsing the MDX source — whatever
 * `rehype-slug` actually gave a heading is what the TOC links to, so the two
 * can never disagree. */
function TableOfContents({
  contentRef,
  activeSlug,
  contentVersion,
}: {
  readonly contentRef: RefObject<HTMLDivElement | null>;
  readonly activeSlug: string | undefined;
  /** Bumped by `ContentMountedSignal` once the active page's lazy chunk has
   * actually mounted — see that component's header for why `activeSlug`
   * alone is not enough to know when to re-scan. */
  readonly contentVersion: number;
}) {
  const [items, setItems] = useState<TocItem[]>([]);

  useEffect(() => {
    const container = contentRef.current;
    if (!container) {
      setItems([]);
      return;
    }
    const headings = Array.from(container.querySelectorAll<HTMLElement>('h2, h3'));
    setItems(
      headings
        .filter((heading) => heading.id !== '')
        .map((heading) => ({ id: heading.id, text: heading.textContent ?? '' })),
    );
    // Re-scan whenever the active page changes (contentRef's children swap)
    // or the lazy content for that page finishes mounting.
  }, [contentRef, activeSlug, contentVersion]);

  if (items.length === 0) return null;

  return (
    <nav className="docs-toc" aria-label={t('entries.docs.toc.ariaLabel', 'On this page')}>
      <p className="docs-toc__title">{t('entries.docs.toc.title', 'On this page')}</p>
      <ul>
        {items.map((item) => (
          <li key={item.id}>
            <a href={`#${item.id}`}>{item.text}</a>
          </li>
        ))}
      </ul>
    </nav>
  );
}

/** `<Suspense>` fallback shown while a page's lazy MDX chunk
 * (`content-registry.ts`) is still loading. A light skeleton, not a spinner,
 * so the content column doesn't jump height once the real page arrives. */
function DocsPageSkeleton() {
  return (
    <div className="docs-skeleton" aria-hidden="true">
      <div className="docs-skeleton__line docs-skeleton__line--title" />
      <div className="docs-skeleton__line" />
      <div className="docs-skeleton__line" />
      <div className="docs-skeleton__line docs-skeleton__line--short" />
    </div>
  );
}

/** Renders nothing itself; its only job is to fire `onMounted` the moment
 * its own subtree actually commits. Placed as a sibling of the lazy
 * `Content` inside the same `<Suspense>` boundary, remounted on every slug
 * change (the boundary is keyed by `slug`), this is how `App` learns "the
 * lazy page just finished loading and its headings now exist in the DOM" —
 * a plain `useEffect` on `App` itself cannot see that moment, because
 * Suspense resolving a lazy child does not change `App`'s own props/state. */
function ContentMountedSignal({ onMounted }: { readonly onMounted: () => void }) {
  useEffect(() => {
    onMounted();
    // Intentionally once per mount: the parent boundary is keyed by `slug`,
    // so a fresh mount here IS a fresh page load.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return null;
}

function NotFound() {
  return (
    <div className="docs-not-found">
      <h1>{t('entries.docs.notFound.heading', 'Page not found')}</h1>
      <p>
        {t('entries.docs.notFound.body', 'That page does not exist. Go back to')}{' '}
        <a href={toPath('')}>{t('entries.docs.notFound.homeLink', 'the home page')}</a>.
      </p>
    </div>
  );
}

export function App() {
  const [slug, setSlug] = useState<string | undefined>(() => currentSlug());
  const contentRef = useRef<HTMLDivElement>(null);
  // Bumped by `ContentMountedSignal` every time a page's lazy chunk actually
  // finishes mounting — the TOC scan and the scroll-to-anchor effect below
  // both need to run again at that point, not just when `slug` changes (see
  // `ContentMountedSignal`'s header for why `slug` alone isn't enough).
  const [contentVersion, setContentVersion] = useState(0);
  const handleContentMounted = useCallback(() => {
    setContentVersion((version) => version + 1);
  }, []);

  useEffect(() => {
    const teardownHistory = initHistoryListener();
    const unsubscribe = onRouteChange(() => setSlug(currentSlug()));
    document.addEventListener('click', handleDocsLinkClick);
    return () => {
      teardownHistory();
      unsubscribe();
      document.removeEventListener('click', handleDocsLinkClick);
    };
  }, []);

  const entry = slug !== undefined ? getContentEntry(slug) : undefined;
  const { prev, next } = slug !== undefined ? neighbours(slug) : { prev: undefined, next: undefined };
  const Content = entry?.Component;

  // Per-page `document.title` and `<meta name="description">`. Both come
  // straight off the eagerly-loaded frontmatter (`content-registry.ts`), so
  // they update the instant the route changes — no need to wait for the
  // lazy page component itself to load.
  useEffect(() => {
    document.title =
      entry !== undefined
        ? `${entry.frontmatter.title} · ${SITE_NAME}`
        : `${t('entries.docs.notFound.heading', 'Page not found')} · ${SITE_NAME}`;

    const description = entry?.frontmatter.description ?? DEFAULT_DESCRIPTION;
    document.querySelector('meta[name="description"]')?.setAttribute('content', description);
  }, [entry]);

  // Scroll behaviour on navigation: to a heading anchor (`#voice`) once it
  // exists in the DOM, or to the top of the page otherwise. Depends on
  // `contentVersion` (not just `slug`) because a hash target only exists once
  // the lazy page component has actually mounted; this effect therefore runs
  // twice per hash navigation — once immediately (scrolls to top, since the
  // target isn't there yet) and once more when `contentVersion` bumps (finds
  // the now-rendered heading and scrolls to it).
  useEffect(() => {
    const hash = window.location.hash;
    if (entry !== undefined && hash.length > 1) {
      const id = decodeURIComponent(hash.slice(1));
      const target = document.getElementById(id);
      if (target !== null) {
        target.scrollIntoView();
        return;
      }
    }
    window.scrollTo({ top: 0 });
    // `entry` deliberately omitted: it is derived from `slug` on every
    // render, so re-running this effect on `slug` already covers it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [slug, contentVersion]);

  return (
    <div className="docs-shell">
      <header className="docs-topbar">
        <a className="docs-topbar__brand" href={toPath('')}>
          {t('entries.docs.topbar.brand', 'Elitea Docs')}
        </a>
        <SearchBox />
        <span className="docs-topbar__spacer" />
        <ThemeToggle />
      </header>
      <div className="docs-body">
        <NavView activeSlug={slug} />
        <main className="docs-content">
          <div ref={contentRef}>
            {entry !== undefined && Content !== undefined ? (
              <>
                <h1 className="docs-page-title">{entry.frontmatter.title}</h1>
                <Suspense key={slug} fallback={<DocsPageSkeleton />}>
                  <Content components={docsComponents} />
                  <ContentMountedSignal onMounted={handleContentMounted} />
                </Suspense>
              </>
            ) : (
              <NotFound />
            )}
          </div>
          {Content !== undefined ? (
            <footer className="docs-footer-nav">
              {prev !== undefined ? (
                <a className="docs-footer-nav__link" href={toPath(prev.slug)}>
                  ← {prev.title}
                </a>
              ) : (
                <span />
              )}
              {next !== undefined ? (
                <a className="docs-footer-nav__link" href={toPath(next.slug)}>
                  {next.title} →
                </a>
              ) : (
                <span />
              )}
            </footer>
          ) : null}
        </main>
        <TableOfContents contentRef={contentRef} activeSlug={slug} contentVersion={contentVersion} />
      </div>
    </div>
  );
}
