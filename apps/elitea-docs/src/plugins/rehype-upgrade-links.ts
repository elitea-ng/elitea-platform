/**
 * The upgrade notes (the repository's docs/upgrading/*.md, rendered on
 * /deployment/upgrading) link the way a file on GitHub links: relative to
 * docs/upgrading/. In those files only, this turns a link to a docs page's
 * source (`../../apps/elitea-docs/src/content/docs/<slug>.mdx`) into the
 * page's root-relative path (`/<slug>`, which rehype-base-links then
 * prefixes with the base), and any other relative link into the file on
 * GitHub. It runs before rehype-base-links.
 */
import type { Element, Root } from 'hast';
import { posix } from 'node:path';
import { visit } from 'unist-util-visit';

const REPOSITORY = 'https://github.com/elitea-ng/elitea-platform/blob/main/';
const FRAGMENTS = 'docs/upgrading/';
const PAGES = 'apps/elitea-docs/src/content/docs/';

/** The repository-relative path of a link written in docs/upgrading/, or undefined for an absolute or anchor-only one. */
function repositoryPath(href: string): string | undefined {
  if (href === '' || href.startsWith('#') || href.startsWith('/') || /^[a-z][a-z0-9+.-]*:/i.test(href)) return undefined;
  return posix.normalize(posix.join(FRAGMENTS, href));
}

export function upgradeLink(href: string): string {
  const [path = '', anchor] = href.split('#', 2);
  const resolved = repositoryPath(path);
  if (resolved === undefined) return href;
  const hash = anchor === undefined ? '' : `#${anchor}`;
  if (resolved.startsWith(PAGES) && resolved.endsWith('.mdx')) {
    const slug = resolved.slice(PAGES.length, -'.mdx'.length).replace(/(^|\/)index$/, '');
    return `/${slug}${hash}`;
  }
  return `${REPOSITORY}${resolved}${hash}`;
}

export function rehypeUpgradeLinks() {
  return (tree: Root, file: { path?: string }) => {
    if (!file.path?.replace(/\\/g, '/').includes(`/${FRAGMENTS}`)) return;
    visit(tree, 'element', (node: Element) => {
      const href = node.properties['href'];
      if (node.tagName === 'a' && typeof href === 'string') node.properties['href'] = upgradeLink(href);
    });
  };
}
