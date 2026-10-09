import DOMPurify from 'dompurify';

/**
 * Tags that can execute code, inject styles, or load external resources.
 * `svg`/`math` are included because both namespaces have their own XSS
 * vectors (e.g. `<svg onload=...>`, `<math>` namespace-confusion attacks).
 * Ported from
 * `apps/elitea-ui/src/[fsd]/shared/lib/constants/markdown.constants.js`'s
 * `FORBIDDEN_HTML_TAGS`.
 */
export const FORBIDDEN_MARKDOWN_HTML_TAGS = [
  'script',
  'style',
  'iframe',
  'object',
  'embed',
  'link',
  'meta',
  'base',
  'noscript',
  'svg',
  'math',
  // Form controls: nothing rendered from markdown submits a form. The one
  // input that survives is the read-only task-list checkbox, which is let
  // through by `sanitizeUntrustedHtml` below, not by this list.
  'form',
  'button',
  'select',
  'option',
  'optgroup',
  'datalist',
  'textarea',
  'fieldset',
  'output',
] as const;

/**
 * Attributes that never survive sanitising.
 *
 *  - `style`: inline CSS can overlay or hide the page it is rendered in.
 *  - `class`, `id`, `name`: let markup adopt the application's own selectors
 *    or shadow document properties. No renderer of this HTML reads any of
 *    them (the markdown styles in `DefaultMarkdown`/`Markdown` select by tag
 *    name only, and no highlighter, math or heading-anchor pass runs on it).
 *  - `formaction`, `action`, `form`: form-submission targets and association.
 *
 * `on*` handlers are removed by DOMPurify itself; the contract test proves it.
 */
export const FORBIDDEN_MARKDOWN_HTML_ATTRS = [
  'style',
  'class',
  'id',
  'name',
  'formaction',
  'action',
  'form',
] as const;

/**
 * The ONE place `marked`-rendered HTML is sanitized before it reaches
 * `dangerouslySetInnerHTML` (`shared/ui/DefaultMarkdown/DefaultMarkdown.tsx`,
 * and — through `Token`/`Markdown` composing it —
 * `shared/ui/TooltipMarkdownContent/TooltipMarkdownContent.tsx`). This is a
 * real XSS surface: the markdown rendered here comes from user or
 * AI-generated chat content, not a trusted CMS, so both halves of the
 * defence matter:
 *
 *  - DOMPurify's OWN defaults strip every `on*` event-handler attribute and
 *    reject `javascript:`/unknown-protocol URLs in `href`/`src` (see the
 *    contract tests in `sanitizeHtml.contract.test.ts`).
 *  - `FORBID_ATTR`/`ALLOW_DATA_ATTR: false` remove inline `style`, `class`,
 *    `id`, `name`, `data-*` and the form-submission attributes (see
 *    `FORBIDDEN_MARKDOWN_HTML_ATTRS`). Those are "safe HTML" to a general
 *    sanitiser, not to chat/AI output rendered inside the product's own page.
 *  - `FORBID_TAGS` closes the remaining hole DOMPurify leaves open BY
 *    DESIGN: `<script>`/`<style>`/`<iframe>`/... are "safe HTML" to a
 *    general-purpose sanitizer unless told otherwise — safe for a trusted
 *    CMS author, not safe for arbitrary chat/AI output.
 *  - `ADD_ATTR: ['target']` is the one attribute allow-listed on top of
 *    DOMPurify's own safe defaults. DOMPurify strips `target` by default
 *    (a bare `target="_blank"` is a reverse-tabnabbing vector via
 *    `window.opener`); `DefaultMarkdown.tsx`'s link renderer always pairs
 *    it with `rel="noopener noreferrer"` (which DOMPurify already allows),
 *    so allow-listing `target` here does not reopen that hole — this is
 *    DOMPurify's own documented recipe for "open links in a new tab".
 */
export function sanitizeMarkdownHtml(html: string): string {
  return sanitizeUntrustedHtml(html, { taskCheckbox: true });
}

/** The only attributes a kept task-list checkbox may carry. */
const TASK_CHECKBOX_ATTRS = new Set(['type', 'checked', 'disabled']);

/**
 * The single DOMPurify configuration behind both HTML sanitisers
 * (`sanitizeMarkdownHtml`, `sanitizeSplashHtml`).
 *
 * `taskCheckbox: true` keeps ONLY `<input type="checkbox" disabled>` (what GFM
 * task lists render), stripped down to `type`/`checked`/`disabled`; every other
 * `<input>` is removed. DOMPurify has no per-tag attribute allow-list, so that
 * narrowing is a pass over the sanitised (inert) DOM before it is serialised.
 * With `false`, `input` is forbidden outright.
 */
export function sanitizeUntrustedHtml(html: string, options: { taskCheckbox: boolean }): string {
  const forbidTags: string[] = [...FORBIDDEN_MARKDOWN_HTML_TAGS];
  if (!options.taskCheckbox) forbidTags.push('input');
  const config = {
    FORBID_TAGS: forbidTags,
    FORBID_ATTR: [...FORBIDDEN_MARKDOWN_HTML_ATTRS],
    ADD_ATTR: ['target'],
    ALLOW_DATA_ATTR: false,
  };
  if (!options.taskCheckbox) return DOMPurify.sanitize(html, config);

  const body = DOMPurify.sanitize(html, { ...config, RETURN_DOM: true as const }) as HTMLElement;
  for (const input of Array.from(body.querySelectorAll('input'))) {
    const isTaskCheckbox =
      input.getAttribute('type')?.toLowerCase() === 'checkbox' && input.hasAttribute('disabled');
    if (!isTaskCheckbox) {
      input.remove();
      continue;
    }
    for (const attr of Array.from(input.attributes)) {
      if (!TASK_CHECKBOX_ATTRS.has(attr.name)) input.removeAttribute(attr.name);
    }
  }
  return body.innerHTML;
}
