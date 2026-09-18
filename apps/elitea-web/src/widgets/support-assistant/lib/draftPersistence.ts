/**
 * draftPersistence — keeps the Support Assistant's unsent composer draft
 * alive across a page navigation (#935/ELITEA-0623).
 *
 * `AppShell` is mounted PER PAGE (`pages/**` route target wraps its own
 * content in `<AppShell>{...}</AppShell>`, per `AppShell.tsx`'s own module
 * doc) rather than once at the router root, and `SupportAssistantWidget` /
 * `EliteaAssistant` live inside it. A plain module-scope variable (the
 * first cut at this fix) survives a component remount driven by the
 * client-side router, but the E2E proof for #935
 * (`support.entrypoints.spec.ts`, "the session is shared across a page
 * navigation") drives navigation with Playwright's `page.goto(...)`, which
 * is a genuine top-level browser navigation — the same thing a user
 * typing a URL or using back/forward triggers — and that tears down and
 * re-executes the whole JS runtime, wiping any plain module-scope
 * variable along with it (confirmed: a `window.__marker` set before such a
 * `goto` is gone after it, even though react-router-driven in-page
 * navigation would have preserved it). `useChat`'s `inputText` (a plain
 * `useState('')`, `vendor/lib/hooks/chat.hook.ts`) resets with it.
 *
 * So the draft is persisted here in `sessionStorage` (via
 * `shared/lib/storage`'s namespaced wrapper) instead: it is per-tab
 * transient UI state exactly like the module-scope variable was, but it
 * survives a full document reload the way module state cannot, and it
 * stays inside the `el.` namespace so the §5.4 logout sweep
 * (`clearNamespace()`) reaches it — unlike a raw, un-namespaced key would
 * (`browser-storage-escapes-logout-sweep`). Moving the widget's mount
 * point up to the router root remains the real, non-minimal fix for the
 * class of bug ("the widget resets on navigation"); this file targets only
 * the one symptom the pinned case names — the unsent draft.
 *
 * `createStorage('session')` is called FRESH inside each function, never
 * cached at this module's top level — see
 * `widgets/sidebar/lib/collapsedPersistence.ts`'s header for why that
 * matters under vitest 4 + Node 24 (a module-scope call can capture
 * `undefined` before a test's storage shim has run).
 */
import { createStorage } from '@/shared/lib/storage';

const DRAFT_KEY = 'support.draft';

export function getPersistedDraft(): string {
  return createStorage('session').get(DRAFT_KEY) ?? '';
}

export function setPersistedDraft(value: string): void {
  const storage = createStorage('session');
  if (value === '') {
    storage.remove(DRAFT_KEY);
    return;
  }
  storage.set(DRAFT_KEY, value);
}
