/**
 * `beforeLoad` for the desktop-only routes: outside the `desktop` build there
 * is no such screen, so the visitor goes to `/chat`. The test is a literal
 * `import.meta.env.MODE` comparison on purpose (see
 * `pages/workspace/desktopEntry.tsx`).
 */
import { redirect } from '@tanstack/react-router';

export function requireDesktopBuild(): void {
  if (import.meta.env.MODE !== 'desktop') {
    // oxlint-disable-next-line typescript/only-throw-error -- TanStack Router's beforeLoad redirect contract: throw the Response redirect() returns, not an Error.
    throw redirect({ to: '/chat' });
  }
}
