import { anonymousFetch, type AnonymousFetchOptions } from '@/api/anonymous';
import { originOf } from '@/api/paths';
import { log } from '@/lib/log';

/**
 * Workspace SVG logos, loaded so that nothing outside the workspace origin is
 * ever fetched because of them.
 *
 * `SvgUri` is not used: it downloads with a plain `fetch` (which follows a
 * redirect to any host) and renders whatever the document references, and an
 * SVG can pull in remote content of its own (`<image href="https://…">`,
 * `fill="url(https://…)"`, `<foreignObject>`). Instead the document is
 * fetched through the origin-guarded anonymous fetch (a cross-origin redirect
 * is refused) and rendered with `SvgXml` only when every reference in it is
 * local to the document (`#id`). Anything else refuses the whole logo; it is
 * decoration, and the screen simply shows none.
 */

export const MAX_SVG_BYTES = 400_000;

const FORBIDDEN_ELEMENT =
  /<\s*(?:[a-z][\w.-]*:)?(?:script|foreignObject|image|feImage|iframe|object|embed|audio|video|a)\b/i;
const QUOTED_HREF = /\b(?:[a-z][\w.-]*:)?href\s*=\s*(?:"([^"]*)"|'([^']*)')/gi;
const ANY_HREF = /\b(?:[a-z][\w.-]*:)?href\s*=/gi;
const URL_REFERENCE = /url\(\s*(['"]?)([^'")]*)/gi;

const local = (reference: string) => reference.trim().startsWith('#');

/** The document when it references nothing outside itself, else null. */
export function sanitizeBrandSvg(xml: string): string | null {
  if (typeof xml !== 'string' || xml.length === 0 || xml.length > MAX_SVG_BYTES) return null;
  if (!/<svg[\s>]/i.test(xml)) return null;
  // DTDs: external entities and entity expansion.
  if (/<!(?:DOCTYPE|ENTITY)/i.test(xml)) return null;
  if (FORBIDDEN_ELEMENT.test(xml)) return null;
  if (/@import|\bsrc\s*=/i.test(xml)) return null;
  const hrefs = [...xml.matchAll(QUOTED_HREF)];
  // An unquoted (or otherwise unparsed) href is refused rather than guessed at.
  if (hrefs.length !== [...xml.matchAll(ANY_HREF)].length) return null;
  if (!hrefs.every((match) => local(match[1] ?? match[2] ?? ''))) return null;
  if (![...xml.matchAll(URL_REFERENCE)].every((match) => local(match[2]))) return null;
  return xml;
}

/** Fetches a vetted same-origin SVG logo; null on any failure or refusal. */
export async function fetchBrandSvg(uri: string, options: AnonymousFetchOptions = {}): Promise<string | null> {
  const origin = originOf(uri);
  if (origin === null) return null;
  try {
    const response = await anonymousFetch(origin, uri, {}, options);
    if (response.status !== 200) return null;
    const text = await response.text();
    const safe = sanitizeBrandSvg(text);
    if (safe === null) log.info('brand logo refused (references outside the document)');
    return safe;
  } catch (error) {
    log.info('brand logo unavailable', { error });
    return null;
  }
}
