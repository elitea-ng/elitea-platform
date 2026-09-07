import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import type { Tokens } from 'marked';

import { DefaultMarkdown } from '../DefaultMarkdown';
import { isPlainTextToken, spokenOverlapWithin, splitForHighlight, type SpokenRange } from './spokenRange';

/**
 * The one word (or few) currently being read aloud, wrapped in a `<mark>` —
 * plain-text spans on either side, never HTML. `Token` renders this instead
 * of `DefaultMarkdown` for a 'paragraph'/'text' token `isPlainTextToken`
 * clears AND `spokenOverlapWithin` finds non-empty (issue #625 item 1).
 */
function SpokenHighlight({
  raw,
  overlap,
  component = 'span',
}: {
  readonly raw: string;
  readonly overlap: SpokenRange;
  /** 'p' for a block-mode 'paragraph' token, so the surrounding `& p {…}` paragraph spacing still applies. */
  readonly component?: 'p' | 'span';
}): ReactNode {
  const { before, highlighted, after } = splitForHighlight(raw, overlap);
  return (
    <Box component={component} data-testid="spoken-highlight">
      {before}
      <Box
        component="mark"
        sx={(theme: Theme) => ({
          backgroundColor: theme.vars.palette.warning.light,
          color: 'inherit',
          borderRadius: theme.vars.shape.radiusSm,
        })}
      >
        {highlighted}
      </Box>
      {after}
    </Box>
  );
}

/** `undefined` unless `startPos` is known, `token` is plain text, and `spokenRange` reaches into its span. */
function overlapFor(
  token: { readonly raw: string },
  spokenRange: SpokenRange | undefined,
  startPos: number | undefined,
): SpokenRange | undefined {
  return startPos === undefined ? undefined : spokenOverlapWithin(spokenRange, startPos, token.raw.length);
}

/**
 * `Token`'s 'paragraph' case: block mode, so `marked.parse(token.text)` would
 * normally re-wrap the source in a real `<p>` (see `DefaultMarkdown`) — the
 * highlighted branch renders a `<p>`-equivalent `Box` itself instead, to keep
 * the surrounding `& p {…}` spacing rule matching either way. Split out of
 * `Token.tsx` (issue #625 item 1) to hold that file under the §3.5
 * file-length budget, and out of `renderContainerToken`'s own switch to hold
 * ITS cyclomatic complexity under budget too.
 */
export function renderParagraphToken(
  token: Tokens.Paragraph,
  renderHtml: boolean,
  spokenRange: SpokenRange | undefined,
  startPos: number | undefined,
): ReactNode {
  const overlap = isPlainTextToken(token) ? overlapFor(token, spokenRange, startPos) : undefined;
  if (overlap) {
    return (
      <SpokenHighlight
        raw={token.raw}
        overlap={overlap}
        component="p"
      />
    );
  }
  return (
    <DefaultMarkdown
      markdown={token.text}
      renderHtml={renderHtml}
    />
  );
}

/**
 * `Token`'s 'text' case: inline mode, a TIGHT list item's content wrapper —
 * GFM renders tight-list content without a `<p>` wrapper, unlike a loose
 * list's 'paragraph' tokens. See `renderParagraphToken` for why this lives
 * here rather than in `Token.tsx`.
 */
export function renderTextToken(
  token: Tokens.Text,
  renderHtml: boolean,
  spokenRange: SpokenRange | undefined,
  startPos: number | undefined,
): ReactNode {
  const overlap = isPlainTextToken(token) ? overlapFor(token, spokenRange, startPos) : undefined;
  if (overlap) {
    return (
      <SpokenHighlight
        raw={token.raw}
        overlap={overlap}
      />
    );
  }
  return (
    <DefaultMarkdown
      markdown={token.text}
      inline
      renderHtml={renderHtml}
    />
  );
}
