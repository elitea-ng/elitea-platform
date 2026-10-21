/**
 * Links that leave the app open in the system browser.
 *
 * The desktop webview is privileged and only ever shows bundled assets, so a
 * `target="_blank"` link or `window.open` must not open a second webview (which
 * would be a navigation to remote content). They go to the user's browser
 * through the opener plugin instead. Only http and https are ever handed over.
 * The one window the page opens itself is an inspector tab's own window
 * (`panelWindows.ts`): an empty document of the page's origin, no navigation.
 * Its links are the page's too, so the frame hands each such document to
 * `installExternalLinksIn` while it fills it.
 *
 * A relative `href` resolves against the DEPLOYMENT origin, not the page's own
 * location: the page is the bundled `tauri://` (or `http://tauri.localhost`)
 * document, and a relative link in deployment content means the deployment.
 */
import type { HostBridge } from './hostBridge';
import { isPanelWindowUrl } from './panelWindows';

function isWebUrl(raw: string, base: string): URL | undefined {
  try {
    const url = new URL(raw, base);
    return url.protocol === 'http:' || url.protocol === 'https:' ? url : undefined;
  } catch {
    return undefined;
  }
}

/** Installs the click handler on a further document (a panel window's); set while `installExternalLinks` is. */
let installIn: ((doc: Document) => () => void) | null = null;

function linkClicks(bridge: Pick<HostBridge, 'openExternal'>, origin: string, doc: Document): () => void {
  const onClick = (event: MouseEvent): void => {
    const anchor = (event.target as Element | null)?.closest?.('a[href]');
    // `HTMLAnchorElement` of the anchor's own window: a panel window's elements are not the main window's instances.
    const Anchor = anchor?.ownerDocument.defaultView?.HTMLAnchorElement ?? HTMLAnchorElement;
    if (!(anchor instanceof Anchor) || anchor.target !== '_blank') return;
    event.preventDefault();
    const url = isWebUrl(anchor.getAttribute('href') ?? '', origin);
    if (url !== undefined) void bridge.openExternal(url.href).catch(() => undefined);
  };
  doc.addEventListener('click', onClick);
  return () => doc.removeEventListener('click', onClick);
}

/** Sends `doc`'s `target="_blank"` links to the system browser like the main window's; a no-op outside the desktop app. */
export function installExternalLinksIn(doc: Document): () => void {
  return installIn === null ? () => undefined : installIn(doc);
}

export function installExternalLinks(
  bridge: Pick<HostBridge, 'openExternal'>,
  /** The connected deployment origin: the base for relative links. */
  origin: string,
  doc: Document = document,
  win: Window = window,
): () => void {
  const unlink = linkClicks(bridge, origin, doc);
  const into = (other: Document): (() => void) => linkClicks(bridge, origin, other);
  installIn = into;

  const originalOpen = win.open.bind(win);
  win.open = (url?: string | URL, name?: string, features?: string) => {
    if (url !== undefined && isPanelWindowUrl(String(url))) return originalOpen(String(url), name, features);
    const target = url === undefined ? undefined : isWebUrl(String(url), origin);
    if (target !== undefined) void bridge.openExternal(target.href).catch(() => undefined);
    return null;
  };

  return () => {
    unlink();
    win.open = originalOpen;
    if (installIn === into) installIn = null;
  };
}
