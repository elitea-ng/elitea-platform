/**
 * Web push routes (ADR-0045 decision 6). Thin wrappers over the generated
 * client that unwrap the `{data, status, headers}` envelope.
 * `project_id` is any project the person can read: it gives the routes their
 * usual shape and auth; subscriptions belong to the person, not the project.
 */
import {
  createPushSubscription,
  deletePushSubscription as deletePushSubscriptionOp,
  getPushConfig as getPushConfigOp,
} from '@/shared/api/generated/notifications/notifications';
import type { PushConfig, PushSubscriptionRequest, PushSubscriptionSaved } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

export type { PushConfig };
export type PushSubscriptionBody = PushSubscriptionRequest;

export async function getPushConfig(projectId: string | number): Promise<PushConfig> {
  return unwrapBody(await getPushConfigOp(String(projectId))) as PushConfig;
}

/** 201 for a new endpoint, 200 when the endpoint was known (its keys are updated). */
export async function postPushSubscription(projectId: string | number, body: PushSubscriptionBody): Promise<PushSubscriptionSaved> {
  return unwrapBody(await createPushSubscription(String(projectId), body)) as PushSubscriptionSaved;
}

/** 204, also when the endpoint is not known. */
export async function deletePushSubscription(projectId: string | number, endpoint: string): Promise<void> {
  await deletePushSubscriptionOp(String(projectId), { endpoint });
}
