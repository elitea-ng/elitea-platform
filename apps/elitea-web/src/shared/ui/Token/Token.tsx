import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import type { MarkedToken } from 'marked';

import { DefaultMarkdown } from '../DefaultMarkdown';
import type { SpokenRange } from '../lib/spokenRange';
import { renderParagraphToken, renderTextToken } from '../lib/SpokenHighlight';
import { t } from '@/shared/i18n';

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface TokenProps {
  /** One node of a `marked.lexer()` token tree (a full token, or a child from `.tokens`/`.items`). */
  token: MarkedToken;
  renderHtml?: boolean;
  /**
   * The word being read aloud (issue #625 item 1) and this token's own
   * `[startPos, …)` offset into the source — both come from `Markdown`'s
   * top-level stamping pass, so a `Token` reached by RECURSION (a list item,
   * a blockquote child, a table cell) never receives them and never
   * highlights. That is a deliberate, documented limit, not a bug: those
   * nested positions are not stamped, so guessing one would risk a
   * highlight landing on the wrong word instead of just not lighting up.
   */
  spokenRange?: SpokenRange | undefined;
  startPos?: number | undefined;
}

// The three-way split below (leaf-inline / simple-structural / container)
// exists ONLY to keep every individual function under the §3.5 cyclomatic-
// complexity budget (12) while `typescript/switch-exhaustiveness-check`
// still requires each `switch` to enumerate every member of the union IT
// switches over: one 21-case switch would cost ~21+ complexity in a single
// function (measured: the pre-split version was 19 with only 14 of the 21
// cases written out). Splitting by `Extract<MarkedToken, {type: ...}>` lets
// each switch's own exhaustiveness obligation shrink to its slice, not the
// full 21.

type LeafInlineToken = Extract<
  MarkedToken,
  { type: 'codespan' | 'del' | 'em' | 'escape' | 'image' | 'link' | 'strong' }
>;
const LEAF_INLINE_TYPES: ReadonlySet<string> = new Set([
  'codespan',
  'del',
  'em',
  'escape',
  'image',
  'link',
  'strong',
]);
function isLeafInlineToken(token: MarkedToken): token is LeafInlineToken {
  return LEAF_INLINE_TYPES.has(token.type);
}

type SimpleStructuralToken = Extract<
  MarkedToken,
  { type: 'space' | 'def' | 'checkbox' | 'hr' | 'br' | 'code' | 'html' }
>;
const SIMPLE_STRUCTURAL_TYPES: ReadonlySet<string> = new Set([
  'space',
  'def',
  'checkbox',
  'hr',
  'br',
  'code',
  'html',
]);
function isSimpleStructuralToken(token: MarkedToken): token is SimpleStructuralToken {
  return SIMPLE_STRUCTURAL_TYPES.has(token.type);
}

type ContainerToken = Extract<
  MarkedToken,
  { type: 'heading' | 'blockquote' | 'list' | 'list_item' | 'table' | 'paragraph' | 'text' }
>;

const HEADING_VARIANT = {
  1: 'headingLarge',
  2: 'headingMedium',
} as const;

/** Depth 3-6 collapse to `headingSmall` — the theme has three heading rungs, the baseline's CSS had six. */
function headingVariant(depth: number): 'headingLarge' | 'headingMedium' | 'headingSmall' {
  return HEADING_VARIANT[depth as 1 | 2] ?? 'headingSmall';
}

function headingTag(depth: number): 'h1' | 'h2' | 'h3' | 'h4' | 'h5' | 'h6' {
  const clamped = Math.min(Math.max(Math.trunc(depth), 1), 6);
  return `h${clamped}` as 'h1' | 'h2' | 'h3' | 'h4' | 'h5' | 'h6';
}

function headingSx(theme: Theme) {
  return {
    borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
    paddingBottom: '0.4em',
    marginTop: '1.2em',
    marginBottom: '0.4em',
  };
}

function codeBlockSx(theme: Theme) {
  return {
    margin: '0.8em 0',
    padding: '0.75rem 1rem',
    borderRadius: theme.vars.shape.radiusSm,
    background: theme.vars.palette.background.codeMirrorEditor,
    overflowX: 'auto' as const,
  };
}

function tableCellSx(theme: Theme, align: 'center' | 'left' | 'right' | null) {
  return {
    border: `0.0625rem solid ${theme.vars.palette.border.table}`,
    padding: '0.5rem 0.75rem',
    textAlign: align ?? ('left' as const),
  };
}

/** space/def/checkbox render nothing; hr/br/code/html are self-contained (no child-token recursion). */
function renderSimpleToken(token: SimpleStructuralToken, renderHtml: boolean): ReactNode {
  switch (token.type) {
    case 'space':
    case 'def':
    case 'checkbox':
      // 'checkbox' is rendered by its owning 'list_item' case in `renderContainerToken`, not here.
      return null;

    case 'hr':
      return (
        <Box
          component="hr"
          sx={(theme: Theme) => ({
            border: 'none',
            borderTop: `0.0625rem solid ${theme.vars.palette.border.lines}`,
            margin: '1em 0',
          })}
        />
      );

    case 'br':
      return <Box component="br" />;

    case 'code':
      return (
        <Box
          component="pre"
          sx={codeBlockSx}
        >
          <Box
            component="code"
            sx={(theme: Theme) => ({ fontFamily: theme.typography.fontFamilyMono })}
          >
            {token.text}
          </Box>
        </Box>
      );

    case 'html':
      return (
        <DefaultMarkdown
          markdown={token.raw}
          renderHtml={renderHtml}
        />
      );
  }
}

/** heading/blockquote/list/list_item/table/paragraph/text — the cases that recurse into child tokens. */
function renderContainerToken(
  token: ContainerToken,
  renderHtml: boolean,
  spokenRange: SpokenRange | undefined,
  startPos: number | undefined,
): ReactNode {
  switch (token.type) {
    case 'heading':
      return (
        <Typography
          component={headingTag(token.depth)}
          variant={headingVariant(token.depth)}
          sx={headingSx}
        >
          <DefaultMarkdown
            markdown={token.text}
            inline
            renderHtml={renderHtml}
          />
        </Typography>
      );

    case 'blockquote':
      return (
        <Box
          component="blockquote"
          sx={(theme: Theme) => ({
            margin: '0.8em 0',
            paddingLeft: '1em',
            borderLeft: `0.1875rem solid ${theme.vars.palette.border.lines}`,
            color: theme.vars.palette.text.secondary,
          })}
        >
          {token.tokens.map((child, index) => (
            // eslint-disable-next-line react/no-array-index-key -- marked tokens have no stable identity across re-renders
            <Token
              key={index}
              // `Tokens.Blockquote.tokens` is typed `Token[]` (marked's own extension
              // point admits `Tokens.Generic`); this app registers no marked
              // extensions, so every child is actually a `MarkedToken`.
              token={child as MarkedToken}
              renderHtml={renderHtml}
            />
          ))}
        </Box>
      );

    case 'list':
      return (
        <Box
          component={token.ordered ? 'ol' : 'ul'}
          sx={{ margin: '0.5em 0', paddingLeft: '1.5em' }}
        >
          {token.items.map((item, index) => (
            // eslint-disable-next-line react/no-array-index-key -- marked tokens have no stable identity across re-renders
            <Token
              key={index}
              token={item}
              renderHtml={renderHtml}
            />
          ))}
        </Box>
      );

    case 'list_item':
      return (
        <Box
          component="li"
          sx={{ marginBottom: '0.25em' }}
        >
          {token.task && (
            <Box
              component="input"
              type="checkbox"
              checked={token.checked ?? false}
              disabled
              aria-label={
                token.checked
                  ? t('shared.ui.markdown.taskComplete', 'Task complete')
                  : t('shared.ui.markdown.taskIncomplete', 'Task incomplete')
              }
              sx={{ marginInlineEnd: '0.5em' }}
            />
          )}
          {token.tokens.map((child, index) => (
            // eslint-disable-next-line react/no-array-index-key -- marked tokens have no stable identity across re-renders
            <Token
              key={index}
              // Same `Tokens.Generic` extension-point widening as the blockquote case above.
              token={child as MarkedToken}
              renderHtml={renderHtml}
            />
          ))}
        </Box>
      );

    case 'table':
      return (
        <Box
          component="table"
          sx={{ width: '100%', borderCollapse: 'collapse', margin: '0.5em 0' }}
        >
          <Box component="thead">
            <Box component="tr">
              {token.header.map((cell, index) => (
                // eslint-disable-next-line react/no-array-index-key -- marked tokens have no stable identity across re-renders
                <Box
                  key={index}
                  component="th"
                  scope="col"
                  sx={(theme: Theme) => ({
                    ...tableCellSx(theme, cell.align),
                    background: theme.vars.palette.background.secondary,
                  })}
                >
                  <DefaultMarkdown
                    markdown={cell.text}
                    inline
                    renderHtml={renderHtml}
                  />
                </Box>
              ))}
            </Box>
          </Box>
          <Box component="tbody">
            {token.rows.map((row, rowIndex) => (
              // eslint-disable-next-line react/no-array-index-key -- marked tokens have no stable identity across re-renders
              <Box
                key={rowIndex}
                component="tr"
              >
                {row.map((cell, cellIndex) => (
                  // eslint-disable-next-line react/no-array-index-key -- marked tokens have no stable identity across re-renders
                  <Box
                    key={cellIndex}
                    component="td"
                    sx={(theme: Theme) => tableCellSx(theme, cell.align)}
                  >
                    <DefaultMarkdown
                      markdown={cell.text}
                      inline
                      renderHtml={renderHtml}
                    />
                  </Box>
                ))}
              </Box>
            ))}
          </Box>
        </Box>
      );

    case 'paragraph':
      return renderParagraphToken(token, renderHtml, spokenRange, startPos);

    case 'text':
      return renderTextToken(token, renderHtml, spokenRange, startPos);
  }
}

/**
 * Recursively renders one `marked` block-level token tree into semantic
 * MUI elements (`h1`-`h6`, `ul`/`ol`/`li`, `blockquote`, `pre`/`code`,
 * `table`), delegating each LEAF span's own inline markdown (bold/em/
 * links/inline code/inline raw HTML) to `DefaultMarkdown`. Re-architected
 * from `apps/elitea-ui/src/[fsd]/shared/ui/markdown/Token.jsx` — see
 * `DefaultMarkdown`'s doc comment for why leaf content is resolved as one
 * `marked.parseInline` call per span instead of walking marked's own
 * already-split inline sub-tokens.
 *
 * Deviations from the baseline (features/entities-level concerns `shared/
 * ui` cannot depend on — props/callbacks only, no entity types):
 *  - No `interaction_uuid`/`conversation_uuid`/`onEdit`/`selectedCodeBlockInfo`/
 *    `canvasId`/`tableId`/`messageItemId`/`isStreaming`/`showToolbar`/
 *    `spokenRange` — those drove a code-block copy/edit toolbar, an
 *    interactive editable `MarkdownTableBlock`, and TTS-highlight ranges,
 *    all conversation-entity concerns. Code renders as plain `<pre><code>`
 *    and tables render as plain static `<table>`; a features-level wrapper
 *    can add the interactive chrome around this component if a caller
 *    needs it (flagged as an open question in this unit's final report).
 *  - Headings map to the theme's three heading variants
 *    (`headingLarge`/`Medium`/`Small`) instead of six distinct CSS rungs —
 *    the theme (`shared/brand/typography.ts`, unit T2) only has three.
 */
export function Token({ token, renderHtml = true, spokenRange, startPos }: TokenProps): ReactNode {
  if (isLeafInlineToken(token)) {
    // Defensive fallback for the inline-only token kinds ('strong'/'em'/
    // 'del'/'link'/'image'/'codespan'/'escape') that `MarkedToken`'s union
    // requires this component to cover (`typescript/switch-exhaustiveness-
    // check`, via the two switches below) but that this component's own
    // paragraph/text/list_item cases never actually recurse into (they
    // resolve a whole span's inline content via `DefaultMarkdown` before
    // recursion reaches this deep). Re-parsing `token.raw` still renders it
    // correctly.
    return (
      <DefaultMarkdown
        markdown={token.raw}
        inline
        renderHtml={renderHtml}
      />
    );
  }
  if (isSimpleStructuralToken(token)) {
    return renderSimpleToken(token, renderHtml);
  }
  return renderContainerToken(token, renderHtml, spokenRange, startPos);
}
