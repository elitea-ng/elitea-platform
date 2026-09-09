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
import { useEffect, useRef, useState } from 'react';
import type { RefObject } from 'react';

import { t } from '@/shared/i18n';

import { docsComponents } from './components';
import { getContentEntry } from './content-registry';
import { nav, neighbours, type NavGroup, type NavTab } from './nav';
import { currentSlug, handleDocsLinkClick, initHistoryListener, onRouteChange, toPath } from './router';
import { search, type SearchResult } from './search';

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

  return (
    <div className="docs-topbar__search">
      <input
        type="search"
        placeholder={t('entries.docs.search.placeholder', 'Search docs…')}
        value={query}
        onChange={(event) => {
          const value = event.target.value;
          setQuery(value);
          setResults(search(value));
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
function TableOfContents({ contentRef, activeSlug }: { readonly contentRef: RefObject<HTMLDivElement | null>; readonly activeSlug: string | undefined }) {
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
    // Re-scan whenever the active page changes (contentRef's children swap).
  }, [contentRef, activeSlug]);

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
            {Content !== undefined ? <Content components={docsComponents} /> : <NotFound />}
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
        <TableOfContents contentRef={contentRef} activeSlug={slug} />
      </div>
    </div>
  );
}
