/**
 * The inspector's panel windows (a tab opened in a window of its own): the
 * URL the page opens one with, `about:blank#elitea-panel=<tab>`.
 *
 * The window is an empty document of the page's own origin that the page
 * fills from its React tree (`widgets/desktop-shell` portals the tab into
 * it), so it needs no IPC, no route and no second sign-in. The host
 * (`src-tauri/src/window.rs`) recognises the URL, names the native window
 * `panel-<tab>` (its size and position are remembered under that label) and
 * refuses every other window a page asks for.
 */
const PREFIX = 'about:blank#elitea-panel=';

export function panelWindowUrl(tab: string): string {
  return `${PREFIX}${tab}`;
}

export function isPanelWindowUrl(url: string): boolean {
  return url.startsWith(PREFIX) && /^[a-z-]{1,32}$/.test(url.slice(PREFIX.length));
}

/** The host's label for the tab's window (`plugin:window|set_focus` takes it). */
export function panelWindowLabel(tab: string): string {
  return `panel-${tab}`;
}
