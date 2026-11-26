/**
 * The marked actions of the console and the reason they carry (ADR-0059
 * decision 6, "Dangerous actions"; R3.8b, issue 2614).
 *
 * Which actions are marked is the server's action table, not a list kept here:
 * `./adminDangerousActions.json` is written by a Go golden test
 * (`UPDATE_GOLDEN=1 go test ./internal/api/v2/admin/ -run
 * TestAdminDangerousActionsFileMatchesTheActionTable` in services/elitea-main).
 * A key is "METHOD pattern" or "METHOD pattern action", exactly the action
 * table's `Key()`.
 *
 * An API module sends the reason of a marked action with `reasonFields` (a
 * request with a JSON body) or `reasonHeaders` (a request without one, or a
 * route whose body keys are data). Both take the action's key, so the type
 * checker refuses a key the table does not have, and the source guard
 * (`__tests__/dangerousAction.guard.test.ts`) finds every marked action the
 * console calls by it. The reason itself only comes from
 * `AdminDangerousActionDialog`.
 */
import type { ActionReasonCode } from '@/shared/api/generated/model';

import registry from './adminDangerousActions.json';

/** The key of a marked action of the action table. */
export type DangerousActionKey = keyof typeof registry.actions;

/** The danger level of a marked action. */
export type DangerLevel = 'dangerous' | 'critical';

/** The reason codes of a dangerous action, in the order the console lists them. */
export const ADMIN_REASON_CODES = registry.reason_codes as readonly ActionReasonCode[];

/** The code that needs a reason text. */
export const REASON_CODE_OTHER: ActionReasonCode = 'other';

/** The bound of the reason text, in UTF-8 bytes (the header's bound after percent-decoding). */
export const MAX_REASON_BYTES: number = registry.max_reason_header_bytes;

/** What the dialog collects: a code of `ADMIN_REASON_CODES` and the free text. */
export interface AdminReason {
  readonly code: ActionReasonCode;
  readonly text: string;
}

/** The danger level of a marked action. */
export function dangerLevel(key: DangerousActionKey): DangerLevel {
  return registry.actions[key] === 'critical' ? 'critical' : 'dangerous';
}

const encoder = new TextEncoder();

/** The length of `text` in UTF-8 bytes: what the server bounds after percent-decoding. */
export function reasonBytes(text: string): number {
  return encoder.encode(text).length;
}

/**
 * `text` cut to `MAX_REASON_BYTES` of UTF-8 on a code point boundary, so the
 * percent-encoded header decodes to at most the bound and never to half a
 * character.
 */
export function clampReason(text: string): string {
  if (reasonBytes(text) <= MAX_REASON_BYTES) return text;
  let kept = '';
  let used = 0;
  for (const char of text) {
    const size = reasonBytes(char);
    if (used + size > MAX_REASON_BYTES) break;
    kept += char;
    used += size;
  }
  return kept;
}

/** A write with the reason of its dangerous action. */
export type Reasoned<T> = Readonly<T> & { readonly reason: AdminReason };

/** The body fields of the reason, for a marked request with a JSON body. */
export function reasonFields(
  key: DangerousActionKey,
  reason: AdminReason,
): { readonly reason_code: ActionReasonCode; readonly reason: string } {
  if (registry.headers_only.some((route) => key.startsWith(route))) {
    throw new Error(`${key} carries its reason in the headers: use reasonHeaders`);
  }
  return { reason_code: reason.code, reason: clampReason(reason.text.trim()) };
}

/**
 * The body fields of the reason for a route whose own request has a `reason`
 * (a suspension, a revocation, a release): the server keeps that field, so
 * it records the request's own text, and the dialog's text is added to it
 * rather than lost.
 */
export function reasonFieldsKeepText(
  key: DangerousActionKey,
  reason: AdminReason,
  ownText: string | undefined,
): { readonly reason_code: ActionReasonCode; readonly reason: string } {
  const fields = reasonFields(key, reason);
  const own = (ownText ?? '').trim();
  if (own === '') return fields;
  return { ...fields, reason: fields.reason === '' ? own : `${own}\n${fields.reason}` };
}

/**
 * The request options of the reason, for a marked request without a body:
 * `Elitea-Reason-Code` and `Elitea-Reason`, percent-encoded UTF-8 (a browser
 * refuses a header value outside ISO-8859-1, so a reason typed in Cyrillic
 * would otherwise break the action).
 */
export function reasonHeaders(
  _key: DangerousActionKey,
  reason: AdminReason,
): { readonly headers: Record<string, string> } {
  const text = clampReason(reason.text.trim());
  return {
    headers: {
      'Elitea-Reason-Code': encodeURIComponent(reason.code),
      ...(text === '' ? {} : { 'Elitea-Reason': encodeURIComponent(text) }),
    },
  };
}
