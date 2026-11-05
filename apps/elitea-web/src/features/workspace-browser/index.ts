/**
 * Public API — the desktop browser pane of a Local work thread (ADR-0048
 * decisions 1 and 2): the pane (an inspector tab), its downloads as
 * transcript cards, the event feed and thread clean-up. Reached only from
 * `pages/workspace` and the desktop shell, both in the `desktop` build only.
 */
export { BrowserPane } from './ui/BrowserPane';
export { BrowserDownloadCards } from './ui/BrowserDownloadCards';
export { BrowserIpcProvider, useBrowserHost } from './model/ipcContext';
export { useBrowserPanes, useBrowserPane, foldBrowserEvent, EMPTY_PANE, type PaneState } from './model/browserPane.store';
export { useBrowserEvents, forgetThreadBrowser } from './model/useBrowserEvents';
