/**
 * Web push at sign-out (ADR-0045 decision 6). A push subscription belongs to
 * the BROWSER, not the session: left alone, a shared browser keeps showing
 * the previous person's run notices after they signed out. So the logout
 * (`./auth/logout.ts`) removes it here, before it hands the browser to the
 * server logout — while the session cookie still authorizes the DELETE.
 *
 * Everything it needs comes from the registration itself: the push worker's
 * URL carries the project the push routes are called with
 * (`features/notifications/lib/webPush.ts`'s `webPushPlace`). So this needs
 * no feature state and works from any page.
 *
 * Best effort throughout: the DELETE and the browser unsubscribe run side by
 * side (a slow server cannot keep the browser subscribed), every failure is
 * swallowed, and the caller bounds the whole wait.
 */
import { eliteaFetch } from './generated/mutator';

/** The push service worker's file, at the app's base path. */
export const PUSH_WORKER_FILE = 'push-sw.js';

/** The push subscriptions route (U3 contract §2), relative to the REST base. */
export function pushSubscriptionsPath(projectId: string | number): string {
  return `/notifications/push_subscriptions/prompt_lib/${projectId}`;
}

/** How long a sign-out waits for the teardown before it goes on regardless. */
const PUSH_TEARDOWN_WAIT_MS = 2000;

function deleteOnServer(projectId: string, endpoint: string): Promise<unknown> {
  return eliteaFetch<unknown>(pushSubscriptionsPath(projectId), {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ endpoint }),
  });
}

function pushWorkerUrl(registration: ServiceWorkerRegistration): URL | null {
  const script = (registration.active ?? registration.waiting ?? registration.installing)?.scriptURL;
  if (script === undefined) return null;
  try {
    const url = new URL(script);
    return url.pathname.endsWith(`/${PUSH_WORKER_FILE}`) ? url : null;
  } catch {
    return null;
  }
}

async function teardownOne(registration: ServiceWorkerRegistration): Promise<void> {
  const workerUrl = pushWorkerUrl(registration);
  if (workerUrl === null) return;
  const project = workerUrl.searchParams.get('project') ?? '';
  const subscription = await registration.pushManager.getSubscription().catch(() => null);
  if (subscription !== null) {
    await Promise.allSettled([project === '' ? Promise.resolve() : deleteOnServer(project, subscription.endpoint), subscription.unsubscribe()]);
  }
  await registration.unregister().catch(() => false);
}

/** Unsubscribes, deletes on the server and unregisters every push worker of this origin. Never rejects. */
async function teardownWebPush(serviceWorker: ServiceWorkerContainer): Promise<void> {
  const registrations = await serviceWorker.getRegistrations().catch((): readonly ServiceWorkerRegistration[] => []);
  await Promise.allSettled(registrations.map((registration) => teardownOne(registration)));
}

/**
 * The teardown, bounded by {@link PUSH_TEARDOWN_WAIT_MS}; `null` when this
 * browser has no service worker API (nothing to tear down, so the caller can
 * go on at once).
 */
export function startWebPushTeardown(serviceWorker: ServiceWorkerContainer | undefined = browserServiceWorker(), waitMs = PUSH_TEARDOWN_WAIT_MS): Promise<void> | null {
  if (serviceWorker === undefined) return null;
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, waitMs);
    void teardownWebPush(serviceWorker).finally(() => {
      clearTimeout(timer);
      resolve();
    });
  });
}

function browserServiceWorker(): ServiceWorkerContainer | undefined {
  const nav = (globalThis as { navigator?: Navigator }).navigator;
  return nav !== undefined && 'serviceWorker' in nav ? nav.serviceWorker : undefined;
}
