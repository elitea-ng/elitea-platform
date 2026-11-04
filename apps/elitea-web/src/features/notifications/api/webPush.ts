/**
 * Web push routes (ADR-0045 decision 6, U3 contract §2). A thin, typed layer
 * over `eliteaFetch` until the generated client carries these operations.
 * `project_id` is any project the person can read: it gives the routes their
 * usual shape and auth; subscriptions belong to the person, not the project.
 *
 * `eliteaFetch<T>` returns the `{data, status, headers}` envelope cast to
 * `T`, so every call goes through `unwrap` below and is typed with the
 * envelope — typing it as the body would read `undefined` with a 200.
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';
import { pushSubscriptionsPath } from '@/shared/api/webPushTeardown';

export interface PushConfig {
  /** `false` when the administrator turned web push off or this install has no key. */
  readonly enabled: boolean;
  /** The install's VAPID public key: base64url, an uncompressed P-256 point. */
  readonly public_key: string;
}

export interface PushSubscriptionBody {
  readonly endpoint: string;
  readonly keys: { readonly p256dh: string; readonly auth: string };
}

async function unwrap<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

function pushConfigUrl(projectId: string | number): string {
  return `/notifications/push_config/prompt_lib/${projectId}`;
}

export function getPushConfig(projectId: string | number): Promise<PushConfig> {
  return unwrap<PushConfig>(pushConfigUrl(projectId));
}

/** 201 for a new endpoint, 200 when the endpoint was known (its keys are updated). */
export function postPushSubscription(projectId: string | number, body: PushSubscriptionBody): Promise<{ readonly id: string }> {
  return unwrap<{ readonly id: string }>(pushSubscriptionsPath(projectId), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

/** 204, also when the endpoint is not known. */
export async function deletePushSubscription(projectId: string | number, endpoint: string): Promise<void> {
  await unwrap<unknown>(pushSubscriptionsPath(projectId), {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ endpoint }),
  });
}
