/**
 * draftPersistence — keeps the Support Assistant's unsent composer draft
 * alive across a client-side page navigation (#935/ELITEA-0623).
 *
 * `AppShell` is mounted PER PAGE (`pages/**` route target wraps its own
 * content in `<AppShell>{...}</AppShell>`, per `AppShell.tsx`'s own module
 * doc) rather than once at the router root, and `SupportAssistantWidget` /
 * `EliteaAssistant` live inside it — so a route change unmounts the whole
 * widget tree and mounts a fresh one, and `useChat`'s `inputText` (a plain
 * `useState('')`, `vendor/lib/hooks/chat.hook.ts`) resets with it. Moving the
 * mount point up to the router root is the real, non-minimal fix for the
 * class of bug ("the widget resets on navigation"); this file targets only
 * the one symptom the pinned case names — the unsent draft — with the
 * smallest correct primitive: a plain module-scope variable. It survives a
 * component remount (it is not React state) and does not survive a full page
 * reload (module state resets with the JS runtime), which is exactly the
 * "same SPA session" scope #935 describes.
 *
 * Deliberately NOT sessionStorage/localStorage: the draft is transient
 * per-tab UI state, not data worth surviving a reload, and every other
 * browser-storage key in this app is swept on logout
 * (`browser-storage-escapes-logout-sweep`) — adding a new raw key here would
 * either dodge that sweep or need wiring into it for no real benefit.
 */
let persistedDraft = '';

export function getPersistedDraft(): string {
  return persistedDraft;
}

export function setPersistedDraft(value: string): void {
  persistedDraft = value;
}
