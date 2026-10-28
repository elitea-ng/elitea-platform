/**
 * Pages link to each other root-relative, without the base: `/quick-start`
 * or `/quick-start#sign-in`. This prefixes the site's base (`/docs/` in the
 * web image, `/elitea-platform/` on GitHub Pages) to every such `href` and
 * `src`, so one source serves both targets. Protocol-relative (`//host`),
 * external, `#anchor`-only and already-prefixed URLs are left alone.
 */
import type { Element, Root } from 'hast';
import { visit } from 'unist-util-visit';

export function withBase(url: string, base: string): string {
  if (!url.startsWith('/') || url.startsWith('//')) return url;
  const prefix = base.endsWith('/') ? base.slice(0, -1) : base;
  if (prefix === '' || url === prefix || url.startsWith(`${prefix}/`)) return url;
  return `${prefix}${url}`;
}

export function rehypeBaseLinks({ base }: { base: string }) {
  return (tree: Root) => {
    visit(tree, 'element', (node: Element) => {
      for (const attribute of ['href', 'src'] as const) {
        const value = node.properties[attribute];
        if (typeof value === 'string') node.properties[attribute] = withBase(value, base);
      }
      if (node.tagName === 'a') {
        const href = node.properties['href'];
        if (typeof href === 'string' && /^(https?:|mailto:)/.test(href)) {
          node.properties['target'] = '_blank';
          node.properties['rel'] = 'noopener';
        }
      }
    });
  };
}
