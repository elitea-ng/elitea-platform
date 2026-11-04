/** An in-memory browser for the web push steps (tests): service worker, PushManager and Notification. */
import type { WebPushEnv } from './webPush';

export interface FakePushBrowser {
  env: WebPushEnv;
  permission: NotificationPermission;
  /** What `requestPermission` answers. */
  answer: NotificationPermission;
  registered: { readonly url: string; readonly scope: string } | null;
  subscription: { readonly endpoint: string; readonly key: Uint8Array | null } | null;
  readonly calls: string[];
}

export function createFakePushBrowser(options: { readonly supported?: boolean } = {}): FakePushBrowser {
  const fake: FakePushBrowser = {
    env: undefined as unknown as WebPushEnv,
    permission: 'default',
    answer: 'granted',
    registered: null,
    subscription: null,
    calls: [],
  };
  const pushSubscription = (endpoint: string, key: Uint8Array | null): PushSubscription =>
    ({
      endpoint,
      options: { userVisibleOnly: true, applicationServerKey: key === null ? null : key.slice().buffer },
      toJSON: () => ({ endpoint, keys: { p256dh: 'p-key', auth: 'a-key' } }),
      unsubscribe: () => {
        fake.calls.push('unsubscribe');
        fake.subscription = null;
        return Promise.resolve(true);
      },
    }) as unknown as PushSubscription;
  const pushManager = {
    getSubscription: () => Promise.resolve(fake.subscription === null ? null : pushSubscription(fake.subscription.endpoint, fake.subscription.key)),
    subscribe: (opts: PushSubscriptionOptionsInit) => {
      fake.calls.push('subscribe');
      fake.subscription = { endpoint: 'https://push.example/ep-1', key: opts.applicationServerKey as Uint8Array };
      return Promise.resolve(pushSubscription(fake.subscription.endpoint, fake.subscription.key));
    },
  };
  const registration = {
    active: {},
    installing: null,
    waiting: null,
    pushManager,
    unregister: () => {
      fake.calls.push('unregister');
      fake.registered = null;
      return Promise.resolve(true);
    },
  } as unknown as ServiceWorkerRegistration;
  const serviceWorker = {
    register: (url: string, opts: RegistrationOptions) => {
      fake.calls.push('register');
      fake.registered = { url, scope: opts.scope ?? '' };
      return Promise.resolve(registration);
    },
    getRegistration: (scope: string) => Promise.resolve(fake.registered !== null && fake.registered.scope === scope ? registration : undefined),
  } as unknown as ServiceWorkerContainer;
  const notification = {
    get permission() {
      return fake.permission;
    },
    requestPermission: () => {
      fake.calls.push('requestPermission');
      fake.permission = fake.answer;
      return Promise.resolve(fake.answer);
    },
  };
  const supported = options.supported ?? true;
  fake.env = { serviceWorker: supported ? serviceWorker : undefined, notification, pushManager: supported };
  return fake;
}
