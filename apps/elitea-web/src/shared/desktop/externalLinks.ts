/**
 * Links that leave the app open in the system browser.
 *
 * The desktop webview is privileged and only ever shows bundled assets, so a
 * `target="_blank"` link or `window.open` must not open a second webview (which
 * would be a navigation to remote content). They go to the user's browser
 * through the opener plugin instead. Only http and https are ever handed over.
 */
import type { HostBridge } from './hostBridge';

function isWebUrl(raw: string, base: string): URL | undefined {
  try {
    const url = new URL(raw, base);
    return url.protocol === 'http:' || url.protocol === 'https:' ? url : undefined;
  } catch {
    return undefined;
  }
}

export function installExternalLinks(
  bridge: Pick<HostBridge, 'openExternal'>,
  doc: Document = document,
  win: Window = window,
): () => void {
  const onClick = (event: MouseEvent): void => {
    const anchor = (event.target as Element | null)?.closest?.('a[href]');
    if (!(anchor instanceof HTMLAnchorElement) || anchor.target !== '_blank') return;
    event.preventDefault();
    const url = isWebUrl(anchor.getAttribute('href') ?? '', win.location.href);
    if (url !== undefined) void bridge.openExternal(url.href).catch(() => undefined);
  };
  doc.addEventListener('click', onClick);

  const originalOpen = win.open.bind(win);
  win.open = (url?: string | URL) => {
    const target = url === undefined ? undefined : isWebUrl(String(url), win.location.href);
    if (target !== undefined) void bridge.openExternal(target.href).catch(() => undefined);
    return null;
  };

  return () => {
    doc.removeEventListener('click', onClick);
    win.open = originalOpen;
  };
}
