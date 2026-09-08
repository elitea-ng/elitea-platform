/**
 * The sanitiser for operator-authored splash markup — the port of pylon's
 * `splash_template`.
 *
 * ## Why this is not just `sanitizeMarkdownHtml`
 *
 * It is the same DOMPurify call with the same forbid-list, and that is the
 * point: this module exists so the SPLASH's guarantee has a name and a test of
 * its own. `sanitizeMarkdownHtml` is documented as the boundary for
 * `marked`-rendered chat and AI output; the splash body is neither, it is
 * markup an administrator typed, and it reaches EVERY user the maintenance
 * window refuses. Two callers with different threat models sharing one function
 * is how a forbid-list gets relaxed for the easier of them.
 *
 * ## Why the browser sanitises at all when the server already refused
 *
 * The server refuses executable markup on the way in
 * (services/elitea-main internal/api/v2/admin/config_values.go's
 * `validateSplashHTML`). That check keeps script out of a row that outlives
 * every renderer. It does not make this one redundant: a row written before
 * that check existed, restored from a backup, or written by a future path that
 * forgets it, still has to arrive on screen harmless. The check that runs
 * closest to `dangerouslySetInnerHTML` is the one that cannot be bypassed by
 * anything upstream.
 *
 * ## What survives
 *
 * Ordinary body markup: paragraphs, lists, tables, emphasis, and links (with
 * `target`, which `DefaultMarkdown` also allows). What does not: `script`,
 * `style`, `iframe`, `object`, `embed`, `link`, `meta`, `base`, `noscript`,
 * `svg`, `math`, every `on*` handler and every `javascript:` URL — the last two
 * by DOMPurify's own defaults.
 *
 * `<style>` going is a deliberate loss against pylon, whose default splash was
 * a whole document styled by one inline block. Here the body is rendered INSIDE
 * the product's own themed page, so it inherits the platform's typography and
 * colours rather than bringing its own — and an operator who could ship CSS
 * could hide the page it is rendered on.
 */
import DOMPurify from 'dompurify';

import { FORBIDDEN_MARKDOWN_HTML_TAGS } from './sanitizeMarkdownHtml';

export function sanitizeSplashHtml(html: string): string {
  return DOMPurify.sanitize(html, {
    FORBID_TAGS: [...FORBIDDEN_MARKDOWN_HTML_TAGS],
    ADD_ATTR: ['target'],
  });
}
