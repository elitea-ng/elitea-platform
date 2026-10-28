import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import { marked } from 'marked';

import { combineSx } from '../lib/combineSx';
import { sanitizeMarkdownHtml } from '../lib/sanitizeMarkdownHtml';

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface DefaultMarkdownProps {
  /** Raw markdown (or a single span/leaf of markdown) to parse, sanitize, and render. */
  markdown: string;
  /**
   * When `false`, HTML written literally in `markdown`'s source is dropped
   * instead of rendered — markdown-native formatting (`**bold**`, links,
   * lists, ...) still renders. Defaults to `true`.
   */
  renderHtml?: boolean;
  /**
   * Parse as a single inline span (`marked.parseInline` — no wrapping
   * `<p>`) instead of a full block (`marked.parse`). Defaults to `false`.
   */
  inline?: boolean;
  sx?: SxProps<Theme>;
  'data-testid'?: string;
}

/**
 * Turns one span of markdown into sanitized, rendered HTML. This is the
 * ONE place across the `Markdown`/`Token`/`DefaultMarkdown` family that
 * calls `marked` and `dangerouslySetInnerHTML` — `Token` (block-level
 * structure) and `TooltipMarkdownContent` (via `Markdown`) both funnel
 * their leaf content through this component so there is exactly one
 * sanitize-before-render boundary to audit, not one per call site.
 * Re-architected from
 * `apps/elitea-ui/src/[fsd]/shared/ui/markdown/DefaultMarkdown.jsx`, which
 * wrapped the `mui-markdown` package (not part of this app's dependency
 * set — S1 installed `marked`/`dompurify` directly for this batch instead,
 * per the unit brief).
 *
 * Security: `marked.parse`/`parseInline` produce an HTML STRING; that
 * string is ALWAYS run through `sanitizeMarkdownHtml` (DOMPurify, with the
 * baseline's `FORBIDDEN_HTML_TAGS` forbid-list) before it reaches
 * `dangerouslySetInnerHTML` below — see that function's own doc comment
 * and `lib/sanitizeMarkdownHtml.test.ts` for the exact defence and its
 * mutation-proof.
 *
 * Deviation from the baseline: raw inline HTML (`<b>`/`</b>` etc.) is
 * tokenized by `marked` as SEPARATE open/close tokens; the baseline's
 * `Token.jsx` sanitized each one independently
 * (`DOMPurify.sanitize(markedToken.raw, ...)` on a lone `"<b>"`), which
 * DOMPurify normalizes as a *complete* (self-closing) fragment — the pair
 * loses its nesting relationship (verified empirically against the
 * installed `dompurify@3.4.12`: sanitizing `"<b>"` alone round-trips to
 * `"<b></b>"`, an empty tag, not an open tag). Calling `marked.parse`/
 * `parseInline` on the FULL original span (this component) and sanitizing
 * the complete, well-formed result once avoids that pairing bug entirely.
 */
export function DefaultMarkdown({
  markdown,
  renderHtml = true,
  inline = false,
  sx,
  'data-testid': dataTestId,
}: DefaultMarkdownProps): ReactNode {
  const html = useMemo(() => {
    const renderer = new marked.Renderer();
    if (!renderHtml) {
      // Literal HTML in the source is dropped; the surrounding markdown-
      // native text is untouched (marked calls this per raw HTML token, so
      // "A <b>bold</b> B" becomes "A bold B", matching the baseline's
      // `removeHTMLTags` behaviour rather than `<script>`-escaping it).
      renderer.html = () => '';
    } else {
      // Reverse-tabnabbing hardening (R-C1 spirit, matching this app's own
      // `TextWithLink`): every markdown-sourced link opens in a new tab
      // with `rel="noopener noreferrer"`. Post-processes the default
      // renderer's own (already href/title-escaped) output instead of
      // re-deriving the href-cleaning logic marked keeps internal.
      const defaultLink = renderer.link.bind(renderer);
      renderer.link = (token) => {
        const linkHtml = defaultLink(token);
        return linkHtml.startsWith('<a ')
          ? linkHtml.replace('<a ', '<a target="_blank" rel="noopener noreferrer" ')
          : linkHtml;
      };
    }
    const rawHtml = inline
      ? marked.parseInline(markdown, { renderer, async: false })
      // marked ends a block with "\n"; under `Markdown`'s `pre-wrap` that newline is a blank line under every paragraph.
      : marked.parse(markdown, { renderer, async: false }).trimEnd();
    return sanitizeMarkdownHtml(rawHtml);
  }, [markdown, renderHtml, inline]);

  return (
    <Box
      component={inline ? 'span' : 'div'}
      data-testid={dataTestId}
      sx={combineSx(
        (theme: Theme) => ({
          '& a': { color: theme.vars.palette.text.link, textDecoration: 'underline' },
          '& code': { whiteSpace: 'pre-wrap' },
          '& img': { maxWidth: '100%' },
        }),
        sx,
      )}
      // The one sanctioned `dangerouslySetInnerHTML` boundary across the
      // `Markdown`/`Token`/`DefaultMarkdown` family — `html` is always the
      // sanitized output of `sanitizeMarkdownHtml` above, never `rawHtml`
      // directly.
      dangerouslySetInnerHTML={{ __html: html }}
    />
  );
}
