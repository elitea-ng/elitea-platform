import type { ReactNode } from 'react';
import { useEffect, useId, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import { useColorScheme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { combineSx } from '../lib/combineSx';

import type { MermaidDiagramError } from './mermaidError';
import { toDiagramError } from './mermaidError';
import { loadMermaid, sanitizeDiagramSvg } from './mermaidLoader';
import { readMermaidThemeSettings } from './mermaidTheme';

/** @public shared/ui component API. */
export interface MermaidDiagramProps {
  /** Mermaid diagram source, e.g. `graph TD;\n A-->B;`. Empty renders nothing. */
  readonly code: string;
  /**
   * Reports the render outcome as text: the error summary when the diagram
   * fails, `''` when it renders (or when `code` is empty). The component still
   * renders its own error message either way — this is for a CALLER that needs
   * to know, e.g. `features/chat-messages`'s canvas, which only offers its
   * "Quick Fix" control for a diagram that actually failed. Without it a caller
   * cannot tell a broken diagram from a good one, and an always-on repair
   * button next to a correct diagram is noise.
   */
  readonly onError?: (summary: string) => void;
  readonly sx?: SxProps<Theme>;
  readonly 'data-testid'?: string;
}

/**
 * Renders a Mermaid diagram definition as an SVG.
 *
 * Ported from `apps/elitea-ui/src/components/MermaidDiagramOutput/
 * DiagramOutput.jsx` (734 lines). This is the RENDER half of that component and
 * only that half. Three baseline features are deliberately NOT here:
 *
 *  - JPG/PNG/SVG export (`dom-to-image`) and pan/zoom (`svg-pan-zoom`). Both are
 *    extra runtime dependencies bought for a toolbar, not for showing a
 *    diagram; neither consumer needs them to render, and the bundle budget is
 *    the reason to add a dependency only when something actually needs it.
 *  - "Quick Fix" (the model round trip that rewrites broken diagram source). It
 *    is a chat/agent concern, not a `shared/ui` one; the error state below
 *    reports the failure and stops there.
 *
 * SECURITY: diagram source is chat content, so it is untrusted. `securityLevel:
 * 'strict'` is mermaid's strongest level short of `'sandbox'` — HTML inside
 * diagram labels is encoded rather than rendered, and `click` directives cannot
 * bind script. (`'sandbox'` renders into an iframe, which would cut the diagram
 * off from the page's own theme tokens and its measured size; `'loose'`, which
 * the baseline used, actively permits label HTML and is the wrong default for
 * this input.) `htmlLabels: false` removes the `foreignObject` escape hatch, and
 * `sanitizeDiagramSvg` runs DOMPurify over the finished markup before it is
 * injected. See `mermaidLoader.ts` for the full rationale.
 */
export function MermaidDiagram({ code, onError, sx, 'data-testid': dataTestId }: MermaidDiagramProps): ReactNode {
  const containerRef = useRef<HTMLDivElement>(null);
  const [svg, setSvg] = useState('');
  const [error, setError] = useState<MermaidDiagramError | null>(null);
  // Not a scheme branch: the resolved scheme name is a RE-RENDER TRIGGER only.
  // The colours themselves come from computed CSS custom properties
  // (`readMermaidThemeSettings`), which is what keeps this component free of
  // any light/dark fork. Without this dependency a diagram already on screen
  // would keep the previous scheme's colours after a toggle.
  const { colorScheme } = useColorScheme();
  const graphId = `mermaid-${useId().replaceAll(':', '')}`;

  /*
   * `onError` is held in a ref, not read in the effect's dep list. Callers pass
   * an inline arrow far more often than not, and a new identity every parent
   * render would re-run the whole async mermaid render — including its DOM
   * measuring pass — for no reason.
   */
  const onErrorRef = useRef(onError);
  useEffect(() => {
    onErrorRef.current = onError;
  }, [onError]);

  useEffect(() => {
    const container = containerRef.current;
    if (container === null) return;
    if (code.trim() === '') {
      setSvg('');
      setError(null);
      onErrorRef.current?.('');
      return;
    }

    let cancelled = false;
    const render = async (): Promise<void> => {
      const { themeVariables, fontFamily } = readMermaidThemeSettings(container);
      try {
        const mermaid = await loadMermaid();
        if (cancelled) return;
        mermaid.initialize({
          startOnLoad: false,
          securityLevel: 'strict',
          theme: 'base',
          themeVariables,
          htmlLabels: false,
          // Spread rather than `fontFamily` outright: under
          // `exactOptionalPropertyTypes` an explicit `undefined` is not the
          // same as an absent key, and mermaid's own default must win when the
          // environment resolves no font.
          ...(fontFamily === undefined ? {} : { fontFamily }),
        });
        const { svg: rendered } = await mermaid.render(graphId, code);
        if (cancelled) return;
        setSvg(sanitizeDiagramSvg(rendered));
        setError(null);
        onErrorRef.current?.('');
      } catch (caught) {
        if (cancelled) return;
        setSvg('');
        const diagramError = toDiagramError(caught);
        setError(diagramError);
        onErrorRef.current?.(diagramError.summary);
      }
    };

    void render();
    return () => {
      cancelled = true;
      // mermaid appends a scratch `#d<id>` node to <body> while measuring and
      // leaves it behind when render() throws.
      globalThis.document.querySelector(`#d${graphId}`)?.remove();
    };
  }, [code, colorScheme, graphId]);

  return (
    <Box
      ref={containerRef}
      data-testid={dataTestId}
      sx={combineSx({ width: '100%', overflow: 'auto', '& svg': { maxWidth: '100%', height: 'auto' } }, sx)}
    >
      {error !== null && <DiagramErrorMessage error={error} />}
      {error === null && svg !== '' && (
        <Box
          component="figure"
          aria-label={t('shared.mermaid.diagramLabel', 'Diagram')}
          sx={figureSx}
          // The ONE sanctioned innerHTML boundary in this component. `svg` is
          // never set from a raw render result: every write above goes through
          // `sanitizeDiagramSvg` first.
          dangerouslySetInnerHTML={{ __html: svg }}
        />
      )}
    </Box>
  );
}

function DiagramErrorMessage({ error }: { readonly error: MermaidDiagramError }): ReactNode {
  return (
    <Box
      role="alert"
      sx={errorBoxSx}
    >
      <Typography
        variant="labelMedium"
        color="error"
      >
        {error.summary}
      </Typography>
      {error.snippet !== undefined && (
        <Typography
          component="pre"
          variant="bodySmall"
          sx={snippetSx}
        >
          {error.snippet}
        </Typography>
      )}
      <Typography
        variant="bodySmall"
        color="text.secondary"
      >
        {error.hint}
      </Typography>
    </Box>
  );
}

const figureSx: SxProps<Theme> = { margin: 0 };

const errorBoxSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
  padding: '0.5rem',
  border: `1px solid ${theme.vars.palette.border.error}`,
  borderRadius: theme.vars.shape.radiusMd,
  background: theme.vars.palette.background.errorBkg,
});

const snippetSx: SxProps<Theme> = (theme: Theme) => ({
  margin: 0,
  padding: '0.25rem',
  overflowX: 'auto',
  fontFamily: theme.typography.fontFamilyMono,
  whiteSpace: 'pre-wrap',
  borderRadius: theme.vars.shape.radiusSm,
  background: theme.vars.palette.background.default,
  color: theme.vars.palette.text.primary,
});
