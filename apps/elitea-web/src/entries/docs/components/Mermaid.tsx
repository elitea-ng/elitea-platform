/**
 * Renders a ```mermaid fence. NOT part of the MDX component map by tag name
 * the way the rest of the contract is — `mdx/remark-mermaid.ts` is what turns
 * a fenced code block into a literal `<Mermaid chart="...">` element at build
 * time, and this is that element's implementation (registered in
 * `components/index.ts` like any other contract component, since the
 * generated JSX still resolves `Mermaid` through the same component map).
 *
 * `mermaid` is lazy-imported (dynamic `import()`) so the ~600 KB library
 * only loads on a page that actually has a diagram, and is themed from the
 * `data-docs-theme` attribute `App.tsx`'s toggle writes to `<html>` — this
 * entry has no MUI/brand ThemeProvider of its own, so it reads the DOM
 * attribute directly rather than a React context value.
 */
import { useEffect, useId, useState } from 'react';

import { t } from '@/shared/i18n';

export interface MermaidProps {
  readonly chart: string;
}

function currentTheme(): 'dark' | 'default' {
  return document.documentElement.getAttribute('data-docs-theme') === 'dark' ? 'dark' : 'default';
}

export function Mermaid({ chart }: MermaidProps) {
  const rawId = useId();
  const id = `docs-mermaid-${rawId.replace(/[^a-zA-Z0-9]/g, '')}`;
  const [svg, setSvg] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [theme, setTheme] = useState(currentTheme);

  // Re-render when the theme toggle fires (App.tsx dispatches this on every
  // toggle so an open diagram repaints instead of staying stuck in the theme
  // it first rendered under).
  useEffect(() => {
    const onThemeChange = () => setTheme(currentTheme());
    window.addEventListener('docs:theme-change', onThemeChange);
    return () => window.removeEventListener('docs:theme-change', onThemeChange);
  }, []);

  useEffect(() => {
    let cancelled = false;
    async function render() {
      const { default: mermaid } = await import('mermaid');
      mermaid.initialize({ startOnLoad: false, theme, securityLevel: 'strict' });
      try {
        const result = await mermaid.render(id, chart);
        if (!cancelled) setSvg(result.svg);
      } catch (renderError) {
        if (!cancelled) setError(renderError instanceof Error ? renderError.message : String(renderError));
      }
    }
    void render();
    return () => {
      cancelled = true;
    };
  }, [chart, id, theme]);

  if (error !== null) {
    return <pre className="docs-mermaid docs-mermaid--error">{t('entries.docs.mermaid.error', 'Diagram failed to render: {{error}}', { error })}</pre>;
  }
  if (svg === null) {
    return <div className="docs-mermaid docs-mermaid--loading" aria-busy="true" />;
  }
  return (
    // eslint-disable-next-line react/no-danger -- mermaid.render's output is
    // sanitized SVG markup (securityLevel: 'strict'), not raw user input.
    <div className="docs-mermaid" dangerouslySetInnerHTML={{ __html: svg }} />
  );
}
