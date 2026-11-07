/**
 * The streamed cloud browser pane (ADR-0048 decision 7): the web app's cloud
 * threads and the desktop pane's streamed mode render this one component.
 * Import it lazily: only a thread with a cloud browser needs it.
 */
export { CloudBrowserView, type CloudBrowserViewProps } from './CloudBrowserView';
