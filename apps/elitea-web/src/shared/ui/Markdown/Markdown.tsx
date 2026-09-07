import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import { marked, type MarkedToken } from 'marked';

import { combineSx } from '../lib/combineSx';
import { stampTokenPositions, type SpokenRange } from '../lib/spokenRange';
import { Token } from '../Token';

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface MarkdownProps {
  children: string;
  /** When `false`, literal HTML written in `children` is dropped instead of rendered. Defaults to `true`. */
  renderHtml?: boolean;
  sx?: SxProps<Theme>;
  'data-testid'?: string;
  /**
   * The word currently being read aloud, as an offset range into `children`
   * (issue #625 item 1). Omit while text-to-speech is idle — nothing
   * highlights. Only the TOP-LEVEL token whose span contains it highlights;
   * see `isPlainTextToken` for which token types support it.
   */
  spokenRange?: SpokenRange | undefined;
}

/**
 * Lexes `children` into a `marked` block-token tree and renders each token
 * through `Token`. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/markdown/Markdown.jsx` — see
 * `Token`/`DefaultMarkdown`'s doc comments for the sanitize-before-render
 * boundary and the deviations from the baseline's `mui-markdown`-based
 * implementation.
 *
 * `whiteSpace: 'pre-wrap'` cascading to every descendant (matching the
 * baseline's `Markdown.jsx` container styles exactly) is what makes a
 * single source newline read as a visual line break WITHOUT marked's
 * `breaks: true` option converting it to a literal `<br>` — this app
 * renders live-streaming AI responses where the raw text arrives
 * incrementally, and rewriting `\n` to `<br>` server-side-style would fight
 * that streaming re-render.
 */
export function Markdown({
  children,
  renderHtml = true,
  sx,
  'data-testid': dataTestId,
  spokenRange,
}: MarkdownProps): ReactNode {
  const tokens = useMemo(() => marked.lexer(children), [children]);
  // `marked.lexer()`'s return type admits `Tokens.Generic` (marked's own
  // extension point); this app registers no marked extensions, so every
  // lexed token is actually a `MarkedToken`.
  const stamped = useMemo(() => stampTokenPositions(tokens as MarkedToken[]), [tokens]);

  return (
    <Box
      data-testid={dataTestId}
      sx={combineSx(
        {
          whiteSpace: 'pre-wrap',
          '& *': { whiteSpace: 'inherit' },
          '& pre, & code': { whiteSpace: 'pre-wrap' },
          '& p': { margin: '0 0 0.8em' },
          '& p:last-child': { marginBottom: 0 },
        },
        sx,
      )}
    >
      {stamped.map(({ token, startPos }, index) => (
        // eslint-disable-next-line react/no-array-index-key -- marked tokens have no stable identity across re-renders
        <Token
          key={index}
          token={token}
          renderHtml={renderHtml}
          spokenRange={spokenRange}
          startPos={startPos}
        />
      ))}
    </Box>
  );
}
