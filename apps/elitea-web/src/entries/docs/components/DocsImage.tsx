/**
 * The `img` override in the MDX components map (`./index.ts`). The
 * contract prefers `<Screenshot id alt>` (`Screenshot.tsx`) over raw
 * Markdown `![]()` — its `src` already comes from `import.meta.glob`'s
 * build-time URL, which Vite resolves against the active `base` itself, so
 * it needs no help here. This override exists for the raw-`<img>` case the
 * contract does not forbid outright (a hand-written `<img src="/…">` in
 * MDX, or any component that renders one): a root-relative `src` gets BASE
 * prefixed the same way `DocsLink` prefixes a root-relative `href`, so it
 * does not 404 against the site root on a base-prefixed deployment.
 */
import type { ImgHTMLAttributes } from 'react';

import { isExternalHref, withBase } from '../router';

export type DocsImageProps = ImgHTMLAttributes<HTMLImageElement>;

export function DocsImage({ src, alt, ...rest }: DocsImageProps) {
  if (src === undefined || isExternalHref(src) || !src.startsWith('/')) {
    return <img src={src} alt={alt} {...rest} />;
  }
  return <img {...rest} src={withBase(src)} alt={alt} />;
}
