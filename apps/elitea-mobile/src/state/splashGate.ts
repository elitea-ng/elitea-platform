/**
 * Keeps the native splash screen up until the app knows what to show: the
 * session is restored (`hydrated`) and the lock decision is made. Hiding it
 * earlier flashes /connect (or app content) before the real first screen.
 *
 * `createSplashGate(hide)` returns `update(ready)`: `hide` runs once, on the
 * first `ready === true`, and never again.
 */
export function createSplashGate(hide: () => Promise<unknown> | unknown): (ready: boolean) => void {
  let hidden = false;
  return (ready: boolean) => {
    if (!ready || hidden) return;
    hidden = true;
    try {
      void Promise.resolve(hide()).catch(() => {});
    } catch {
      // The splash may already be gone (web, fast refresh).
    }
  };
}
