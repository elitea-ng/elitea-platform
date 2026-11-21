import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { MermaidDiagram } from '.';
import { isMermaidEngineLoaded, sanitizeDiagramSvg } from './mermaidLoader';
import { testTimeout } from '@/test/timeouts';

const VALID = 'graph TD;\n  A[Start] --> B[Finish];';
const INVALID = 'graph TD;\n  A -->';

describe('MermaidDiagram — lazy engine load', () => {
  /**
   * The bundle-budget claim in `mermaidLoader.ts` rests on `mermaid` never
   * being statically imported. A `vi.mock` spy is the usual way to prove that
   * and is banned here (`elitea/no-vi-mock`, R-M1), so the claim is checked
   * two ways instead — the runtime flag, and the source itself, because a
   * green suite with a static import would be exactly the "absence reads as
   * correctness" trap: everything would still WORK, just 500 KB heavier.
   */
  it('has not loaded the engine merely because the module was imported', () => {
    expect(isMermaidEngineLoaded()).toBe(false);
  });

  it('references mermaid only through a dynamic import', () => {
    // vitest serves modules over a non-`file:` URL, so the path is resolved
    // from the vitest root (the app directory) rather than from import.meta.url.
    const source = readFileSync(resolve('src/shared/ui/MermaidDiagram/mermaidLoader.ts'), 'utf8');
    expect(source).toContain("import('mermaid')");
    // No `import … from 'mermaid'` / `export … from 'mermaid'` statement.
    expect(source).not.toMatch(/^\s*(?:import|export)\b[^(\n]*\bfrom\s+['"]mermaid['"]/m);
  });

  it('leaves the engine unloaded for an empty diagram', async () => {
    renderWithTheme(<MermaidDiagram code="   " data-testid="empty" />);
    await waitFor(() => {
      expect(isMermaidEngineLoaded()).toBe(false);
    });
  });
});

describe('MermaidDiagram — rendering', () => {
  it('renders a valid diagram as an inline SVG', async () => {
    const { getByTestId } = renderWithTheme(<MermaidDiagram code={VALID} data-testid="diagram" />);
    await waitFor(
      () => {
        expect(getByTestId('diagram').querySelector('svg')).not.toBeNull();
      },
      { timeout: 15000 },
    );
    expect(getByTestId('diagram').querySelector('[role="alert"]')).toBeNull();
  }, testTimeout(20000));

  it('reports an invalid diagram instead of throwing', async () => {
    const { getByTestId, getByRole } = renderWithTheme(<MermaidDiagram code={INVALID} data-testid="diagram" />);
    await waitFor(
      () => {
        expect(getByRole('alert')).toBeInTheDocument();
      },
      { timeout: 15000 },
    );
    expect(getByRole('alert').textContent).not.toBe('');
    expect(getByTestId('diagram').querySelector('svg')).toBeNull();
  }, testTimeout(20000));

  it('recovers when a broken diagram is replaced by a valid one', async () => {
    const { getByTestId, rerender, queryByRole } = renderWithTheme(
      <MermaidDiagram code={INVALID} data-testid="diagram" />,
    );
    await waitFor(() => {
      expect(queryByRole('alert')).not.toBeNull();
    }, { timeout: 15000 });

    rerender(<MermaidDiagram code={VALID} data-testid="diagram" />);
    await waitFor(() => {
      expect(getByTestId('diagram').querySelector('svg')).not.toBeNull();
    }, { timeout: 15000 });
    expect(queryByRole('alert')).toBeNull();
  }, testTimeout(30000));
});

/**
 * `onError` exists for a caller that must distinguish a broken diagram from a
 * good one — `features/chat-messages`'s canvas only offers its "Quick Fix"
 * control for a diagram that actually failed. The component renders its own
 * error message either way; this is the reporting channel, and the empty
 * string is a real value (it means "this one renders"), not "nothing happened".
 */
describe('MermaidDiagram — onError reporting', () => {
  it('reports the error summary for a broken diagram', async () => {
    const onError = vi.fn();
    renderWithTheme(<MermaidDiagram code={INVALID} onError={onError} data-testid="diagram" />);

    await waitFor(() => expect(onError).toHaveBeenCalled(), { timeout: 15000 });
    const summary = onError.mock.calls.at(-1)?.[0] as string;
    expect(summary).not.toBe('');
  }, testTimeout(20000));

  it('reports the empty string for a diagram that renders', async () => {
    const onError = vi.fn();
    const { getByTestId } = renderWithTheme(
      <MermaidDiagram code={VALID} onError={onError} data-testid="diagram" />,
    );

    await waitFor(() => {
      expect(getByTestId('diagram').querySelector('svg')).not.toBeNull();
    }, { timeout: 15000 });
    expect(onError.mock.calls.at(-1)?.[0]).toBe('');
  }, testTimeout(20000));

  it('clears a previously reported error when the diagram is fixed', async () => {
    const onError = vi.fn();
    const { rerender, queryByRole } = renderWithTheme(
      <MermaidDiagram code={INVALID} onError={onError} data-testid="diagram" />,
    );
    await waitFor(() => expect(queryByRole('alert')).not.toBeNull(), { timeout: 15000 });
    expect(onError.mock.calls.at(-1)?.[0]).not.toBe('');

    rerender(<MermaidDiagram code={VALID} onError={onError} data-testid="diagram" />);
    await waitFor(() => expect(queryByRole('alert')).toBeNull(), { timeout: 15000 });
    expect(onError.mock.calls.at(-1)?.[0]).toBe('');
  }, testTimeout(30000));

  it('reports the empty string for an empty diagram without loading the engine', async () => {
    const onError = vi.fn();
    renderWithTheme(<MermaidDiagram code="   " onError={onError} data-testid="diagram" />);

    await waitFor(() => expect(onError).toHaveBeenCalledWith(''));
  });
});

describe('sanitizeDiagramSvg', () => {
  it('keeps the SVG body and its <style> block', () => {
    const cleaned = sanitizeDiagramSvg('<svg><style>.node{fill:red}</style><g><rect/></g></svg>');
    expect(cleaned).toContain('<svg');
    expect(cleaned).toContain('<style');
    expect(cleaned).toContain('<rect');
  });

  it('strips script, foreignObject and event-handler attributes', () => {
    const cleaned = sanitizeDiagramSvg(
      '<svg><script>window.__pwned = 1</script><foreignObject><b onclick="window.__pwned2 = 1">x</b></foreignObject><rect onload="window.__pwned3 = 1"/></svg>',
    );
    expect(cleaned).not.toContain('<script');
    expect(cleaned).not.toContain('foreignObject');
    expect(cleaned).not.toContain('onclick');
    expect(cleaned).not.toContain('onload');
    expect(cleaned).not.toContain('__pwned');
  });
});
