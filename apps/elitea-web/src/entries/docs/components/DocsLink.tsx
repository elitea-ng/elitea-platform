/**
 * The `a` override in the MDX components map (`./index.ts`). Every Markdown
 * link in content compiles to a plain `<a>` element, resolved against this
 * map's `a` key the same way a capitalised tag like `<Card>` resolves
 * against `Card` (`mdx.d.ts`'s header explains the mechanism). README.md's
 * link convention is a root-relative slug with no docs-base prefix —
 * `[Voice](/menus/chat#voice)` — because content authors do not know
 * whether the built SPA is served at `/docs/` (nginx) or
 * `/elitea-platform/` (GitHub Pages). Without this override that href would
 * reach the DOM verbatim and leave the docs base entirely on click.
 *
 * Three cases, in the order checked:
 *  - external (`isExternalHref`: `http(s)://`, `mailto:`) — a plain link,
 *    always in a new tab (`target="_blank"`, `rel="noopener"`).
 *  - `#anchor`-only — passed through untouched. This is native same-page
 *    scrolling; `router.ts`'s click delegation already leaves a bare `#…`
 *    href alone (`internalTargetUrl` returns `undefined` for it), so there
 *    is nothing to wire up here.
 *  - anything else (the root-relative slug case) — `resolveContentHref`
 *    prefixes it with BASE. The click itself still routes through
 *    `App.tsx`'s single `document`-level `handleDocsLinkClick` listener
 *    (registered once, not per-link) — it already intercepts any same-origin
 *    click under BASE and preserves modifier-click/new-tab/`target`
 *    (`isModifiedClick`/`anchorOptsOut` in router.ts), so this component
 *    does not duplicate that logic; it only has to make the href land
 *    under BASE so the delegation recognizes it as internal at all.
 */
import type { AnchorHTMLAttributes } from 'react';

import { isExternalHref, resolveContentHref } from '../router';

export type DocsLinkProps = AnchorHTMLAttributes<HTMLAnchorElement>;

export function DocsLink({ href, children, ...rest }: DocsLinkProps) {
  if (href === undefined) {
    return <a {...rest}>{children}</a>;
  }

  if (isExternalHref(href)) {
    return (
      <a {...rest} href={href} target="_blank" rel="noopener">
        {children}
      </a>
    );
  }

  return (
    <a {...rest} href={resolveContentHref(href)}>
      {children}
    </a>
  );
}
