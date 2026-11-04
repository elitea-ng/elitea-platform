/**
 * Web push in this browser (ADR-0045 decision 6, U3 contract §4/§5): the
 * opt-in's four states and the subscribe / unsubscribe steps.
 *
 *  - `unsupported` — no service worker or no `PushManager` (an iOS tab that is
 *    not an installed web app, for one). The switch is not shown.
 *  - `blocked` — the person refused notifications for this site in the
 *    browser. Only the browser's site settings can undo that.
 *  - `off` / `on` — `on` means a push subscription exists in this browser.
 *
 * The service worker (`public/push-sw.js`) is registered only when the person
 * turns the switch on, at the app's base path with that path as its scope,
 * and is removed again when they turn it off. The browser APIs come in
 * through {@link WebPushEnv} so the steps can be tested without a browser.
 */
import { PUSH_WORKER_FILE } from '@/shared/api/webPushTeardown';

export type WebPushState = 'unsupported' | 'blocked' | 'off' | 'on';

interface NotificationApi {
  readonly permission: NotificationPermission;
  requestPermission(): Promise<NotificationPermission>;
}

export interface WebPushEnv {
  readonly serviceWorker: ServiceWorkerContainer | undefined;
  readonly notification: NotificationApi | undefined;
  readonly pushManager: boolean;
}

export function browserWebPushEnv(): WebPushEnv {
  const scope = globalThis as { navigator?: Navigator; Notification?: NotificationApi; PushManager?: unknown };
  const serviceWorker = scope.navigator !== undefined && 'serviceWorker' in scope.navigator ? scope.navigator.serviceWorker : undefined;
  return { serviceWorker, notification: scope.Notification, pushManager: scope.PushManager !== undefined };
}

/** Where the worker lives: the app's base path, and the worker URL under it. */
export interface WebPushPlace {
  readonly scope: string;
  readonly scriptUrl: string;
}

/*
 * The worker (`PUSH_WORKER_FILE`) is served from the app's public directory,
 * so it sits at the app's base path (`/app/push-sw.js` in a deployment,
 * `/push-sw.js` in dev) and its default scope covers the whole app. Sign-out
 * finds it by that name (`shared/api/webPushTeardown.ts`).
 */

/**
 * `api` (the REST base, `vite_server_url`) and `project` (a project the person
 * can read, for the route shape) go in the worker URL's query: the worker has
 * no access to the app's runtime config, and it needs both to send a new
 * endpoint after `pushsubscriptionchange`.
 */
export function webPushPlace(basename: string, serverUrl: string, projectId: string): WebPushPlace {
  const trimmed = basename.replace(/\/+$/, '');
  const scope = `${trimmed}/`;
  const query = new URLSearchParams({ api: serverUrl, project: projectId });
  return { scope, scriptUrl: `${scope}${PUSH_WORKER_FILE}?${query.toString()}` };
}

/** A base64url VAPID key (RFC 8292) as the bytes `PushManager.subscribe` takes. */
export function base64UrlToBytes(text: string): Uint8Array<ArrayBuffer> {
  const base64 = text.replace(/-/g, '+').replace(/_/g, '/');
  const padded = base64 + '='.repeat((4 - (base64.length % 4)) % 4);
  const binary = atob(padded);
  const bytes = new Uint8Array(new ArrayBuffer(binary.length));
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

export interface SubscriptionBody {
  readonly endpoint: string;
  readonly keys: { readonly p256dh: string; readonly auth: string };
}

function subscriptionBody(subscription: PushSubscription): SubscriptionBody {
  const json = subscription.toJSON();
  return { endpoint: subscription.endpoint, keys: { p256dh: json.keys?.['p256dh'] ?? '', auth: json.keys?.['auth'] ?? '' } };
}

function supported(env: WebPushEnv): env is WebPushEnv & { serviceWorker: ServiceWorkerContainer; notification: NotificationApi } {
  return env.serviceWorker !== undefined && env.notification !== undefined && env.pushManager;
}

async function currentSubscription(env: WebPushEnv & { serviceWorker: ServiceWorkerContainer }, scope: string): Promise<{ registration: ServiceWorkerRegistration | undefined; subscription: PushSubscription | null }> {
  const registration = await env.serviceWorker.getRegistration(scope);
  const subscription = registration === undefined ? null : await registration.pushManager.getSubscription();
  return { registration, subscription };
}

export async function readWebPushState(env: WebPushEnv, scope: string): Promise<WebPushState> {
  if (!supported(env)) return 'unsupported';
  if (env.notification.permission === 'denied') return 'blocked';
  if (env.notification.permission !== 'granted') return 'off';
  const { subscription } = await currentSubscription(env, scope);
  return subscription === null ? 'off' : 'on';
}

/** A push subscription needs an ACTIVE worker; a fresh registration is still installing. */
function activated(registration: ServiceWorkerRegistration): Promise<void> {
  if (registration.active !== null) return Promise.resolve();
  const worker = registration.installing ?? registration.waiting;
  if (worker === null) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const onChange = (): void => {
      if (worker.state === 'activated') {
        worker.removeEventListener('statechange', onChange);
        resolve();
      } else if (worker.state === 'redundant') {
        worker.removeEventListener('statechange', onChange);
        reject(new Error('the push service worker did not start'));
      }
    };
    worker.addEventListener('statechange', onChange);
  });
}

function sameKey(current: ArrayBuffer | null, expected: Uint8Array): boolean {
  if (current === null) return false;
  const bytes = new Uint8Array(current);
  return bytes.length === expected.length && bytes.every((byte, i) => byte === expected[i]);
}

/**
 * Asks for the browser's permission (when not already given), registers the
 * worker, subscribes with the install's VAPID key and sends the subscription
 * to Main. A refused permission ends at `blocked` (or `off` when the person
 * only closed the prompt). When Main refuses the subscription, the browser's
 * subscription is undone and the error is thrown.
 */
export async function enableWebPush(env: WebPushEnv, place: WebPushPlace, publicKey: string, save: (body: SubscriptionBody) => Promise<unknown>): Promise<WebPushState> {
  if (!supported(env)) return 'unsupported';
  const permission = env.notification.permission === 'granted' ? 'granted' : await env.notification.requestPermission();
  if (permission === 'denied') return 'blocked';
  if (permission !== 'granted') return 'off';
  const registration = await env.serviceWorker.register(place.scriptUrl, { scope: place.scope });
  await activated(registration);
  const applicationServerKey = base64UrlToBytes(publicKey);
  let existing = await registration.pushManager.getSubscription();
  // A subscription made with another key (the install's key changed, or another
  // install once served this origin) is refused by the push service on every
  // send: replace it.
  if (existing !== null && !sameKey(existing.options.applicationServerKey, applicationServerKey)) {
    await existing.unsubscribe().catch(() => false);
    existing = null;
  }
  const subscription = existing ?? (await registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey }));
  try {
    await save(subscriptionBody(subscription));
  } catch (error) {
    await subscription.unsubscribe().catch(() => false);
    throw error;
  }
  return 'on';
}

/**
 * Tells Main to forget this browser's endpoint, then unsubscribes and removes
 * the worker. The browser side is undone even when Main cannot be reached:
 * the push service then answers 404/410 for the endpoint and Main deletes it.
 */
export async function disableWebPush(env: WebPushEnv, scope: string, remove: (endpoint: string) => Promise<unknown>): Promise<WebPushState> {
  if (!supported(env)) return 'unsupported';
  const { registration, subscription } = await currentSubscription(env, scope);
  if (subscription !== null) {
    await remove(subscription.endpoint).catch(() => undefined);
    await subscription.unsubscribe().catch(() => false);
  }
  await registration?.unregister().catch(() => false);
  return env.notification.permission === 'denied' ? 'blocked' : 'off';
}
