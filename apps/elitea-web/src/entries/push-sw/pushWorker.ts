/**
 * Elitea web push service worker (ADR-0045 decision 6, U3 contract §4).
 *
 * `push-sw.ts` installs this on the worker's `self`; the `elitea-push-worker`
 * Vite plugin (vite.config.ts) bundles that entry into one classic script,
 * `push-sw.js`, at the app's base path (`/app/push-sw.js`), so its scope is
 * the whole app. The app registers it only when the person turns on "Web push
 * in this browser" (Settings › Notifications) and removes it when they turn
 * that off (`features/notifications/lib/webPush.ts`).
 *
 * The payload is content-free: a type, ids and an outcome. The notice text is
 * fixed here and nothing is fetched to fill it. The text is English: a worker
 * cannot read the app's catalogue.
 *
 * Self-contained on purpose: it imports nothing at runtime, so the bundle is
 * this file and nothing of the app. `PushWorkerScope` is the part of
 * `ServiceWorkerGlobalScope` it uses, so the tests drive it with a fake.
 */

interface PushPayload {
  readonly type?: string;
  readonly notification_id?: string | number;
  readonly project_id?: string | number;
  readonly conversation_uuid?: string;
  readonly outcome?: string;
}

interface NoticeText {
  readonly title: string;
  readonly body: string;
}

interface WindowClientLike {
  readonly url: string;
  readonly focused: boolean;
  focus(): Promise<WindowClientLike | null>;
  navigate(url: string): Promise<unknown>;
}

interface SubscriptionLike {
  readonly endpoint: string;
  readonly options: PushSubscriptionOptionsInit;
  toJSON(): { readonly keys?: Record<string, string> };
}

interface ExtendableEventLike {
  waitUntil(work: Promise<unknown>): void;
}

interface PushEventLike extends ExtendableEventLike {
  readonly data: { json(): unknown } | null;
}

interface NotificationEventLike extends ExtendableEventLike {
  readonly notification: { readonly data?: { readonly url?: string } | null; close(): void };
}

interface SubscriptionChangeEventLike extends ExtendableEventLike {
  readonly oldSubscription?: SubscriptionLike | null;
  readonly newSubscription?: SubscriptionLike | null;
}

interface PushWorkerEvents {
  install: ExtendableEventLike;
  activate: ExtendableEventLike;
  push: PushEventLike;
  notificationclick: NotificationEventLike;
  pushsubscriptionchange: SubscriptionChangeEventLike;
}

export interface PushWorkerScope {
  readonly location: { readonly href: string };
  addEventListener<K extends keyof PushWorkerEvents>(type: K, listener: (event: PushWorkerEvents[K]) => void): void;
  skipWaiting(): Promise<void> | void;
  fetch(url: string, init: RequestInit): Promise<unknown>;
  readonly clients: {
    matchAll(options: { type: 'window'; includeUncontrolled: boolean }): Promise<readonly WindowClientLike[]>;
    openWindow(url: string): Promise<unknown>;
    claim(): Promise<void>;
  };
  readonly registration: {
    readonly scope: string;
    showNotification(title: string, options: { body: string; tag: string; data: { url: string } }): Promise<void>;
    readonly pushManager: { subscribe(options: PushSubscriptionOptionsInit): Promise<SubscriptionLike> };
  };
}

const TEXT = {
  completed: { title: 'Run finished', body: 'A run is done.' },
  failed: { title: 'Run failed', body: 'A run stopped with an error.' },
  waiting: { title: 'Run needs you', body: 'A run waits for your answer.' },
  limit_reached: { title: 'Run stopped at its limit', body: 'A run reached its chain limit.' },
} as const satisfies Record<string, NoticeText>;

// Shown for a payload this worker cannot read or does not know (a newer
// server's type): the subscription is userVisibleOnly, and a push that shows
// nothing makes the browser show its own notice and can cost the subscription.
const FALLBACK: NoticeText = { title: 'Elitea', body: 'There is new activity.' };

function readPayload(event: PushEventLike): PushPayload {
  try {
    const value = event.data?.json();
    return typeof value === 'object' && value !== null ? value : {};
  } catch {
    return {};
  }
}

function noticeText(payload: PushPayload): NoticeText {
  if (payload.type === 'agent_approval_required') return TEXT.waiting;
  if (payload.type === 'run_finished') return TEXT[payload.outcome as keyof typeof TEXT] ?? TEXT.completed;
  return FALLBACK;
}

function present(value: string | number | undefined): value is string | number {
  return value !== undefined && value !== '';
}

// `/<project>/chat?conversation=<uuid>` selects the project, then opens the conversation (ROUTE-070).
function targetUrl(scope: string, payload: PushPayload): string {
  if (!present(payload.project_id) || !present(payload.conversation_uuid)) return scope;
  return `${scope}${encodeURIComponent(String(payload.project_id))}/chat?conversation=${encodeURIComponent(payload.conversation_uuid)}`;
}

function showsConversation(clientUrl: string, conversationUuid: string | undefined): boolean {
  if (!present(conversationUuid)) return false;
  try {
    const url = new URL(clientUrl);
    return url.pathname.endsWith(`/chat/${conversationUuid}`) || url.searchParams.get('conversation') === conversationUuid;
  } catch {
    return false;
  }
}

function subscriptionBody(subscription: SubscriptionLike): { endpoint: string; keys: { p256dh: string; auth: string } } {
  const keys = subscription.toJSON().keys ?? {};
  return { endpoint: subscription.endpoint, keys: { p256dh: keys['p256dh'] ?? '', auth: keys['auth'] ?? '' } };
}

export function installPushWorker(sw: PushWorkerScope): void {
  const windowClients = (): Promise<readonly WindowClientLike[]> => sw.clients.matchAll({ type: 'window', includeUncontrolled: true });

  async function onPush(payload: PushPayload): Promise<void> {
    const text = noticeText(payload);
    const clients = await windowClients();
    if (clients.some((client) => client.focused && showsConversation(client.url, payload.conversation_uuid))) return;
    const id = present(payload.notification_id) ? payload.notification_id : present(payload.type) ? payload.type : 'activity';
    await sw.registration.showNotification(text.title, {
      body: text.body,
      tag: `elitea-${id}`,
      data: { url: targetUrl(sw.registration.scope, payload) },
    });
  }

  async function onClick(event: NotificationEventLike): Promise<unknown> {
    event.notification.close();
    const scope = sw.registration.scope;
    const url = event.notification.data?.url ?? scope;
    const app = (await windowClients()).find((client) => client.url.startsWith(scope));
    if (app === undefined) return sw.clients.openWindow(url);
    try {
      const focused = await app.focus();
      return await (focused ?? app).navigate(url);
    } catch {
      return sw.clients.openWindow(url);
    }
  }

  // The worker URL carries the REST base and a project the person can read (`webPushPlace`).
  function subscriptionsUrl(): string {
    const params = new URL(sw.location.href).searchParams;
    const api = (params.get('api') ?? '/api/v2').replace(/\/+$/, '');
    return `${api}/notifications/push_subscriptions/prompt_lib/${encodeURIComponent(params.get('project') ?? '')}`;
  }

  function send(method: 'POST' | 'DELETE', body: unknown): Promise<unknown> {
    return sw.fetch(subscriptionsUrl(), {
      method,
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  }

  // The push service replaced or expired the subscription: subscribe again and tell Main the new endpoint.
  async function onSubscriptionChange(event: SubscriptionChangeEventLike): Promise<void> {
    const old = event.oldSubscription ?? null;
    const next = event.newSubscription ?? (old === null ? null : await sw.registration.pushManager.subscribe(old.options));
    if (next === null) return;
    await send('POST', subscriptionBody(next));
    if (old !== null && old.endpoint !== next.endpoint) await send('DELETE', { endpoint: old.endpoint }).catch(() => undefined);
  }

  sw.addEventListener('install', () => {
    void sw.skipWaiting();
  });
  sw.addEventListener('activate', (event) => event.waitUntil(sw.clients.claim()));
  sw.addEventListener('push', (event) => event.waitUntil(onPush(readPayload(event))));
  sw.addEventListener('notificationclick', (event) => event.waitUntil(onClick(event)));
  sw.addEventListener('pushsubscriptionchange', (event) => event.waitUntil(onSubscriptionChange(event)));
}
