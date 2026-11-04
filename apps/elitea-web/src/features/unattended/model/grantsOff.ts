/**
 * The administrator switch `approvals.unattended_grants` can be off. The web
 * cannot read that switch, so it learns it from Main's refusal: 422
 * `unattended_grant_refused` with reason `disabled`. Then the send goes again
 * once without the grant, and the "Run to completion" control hides until the
 * page loads again (this flag lives in memory only).
 */
import { useSyncExternalStore } from 'react';

import { isRecord } from '@/shared/lib/object';

let off = false;
const listeners = new Set<() => void>();

/** True after Main refused a grant because unattended grants are off. */
export function grantsTurnedOff(): boolean {
  return off;
}

/** Record that unattended grants are off: every control hides. */
export function markGrantsTurnedOff(): void {
  if (off) return;
  off = true;
  for (const listener of listeners) listener();
}

/** Test only: forget the flag. */
export function resetGrantsTurnedOff(): void {
  off = false;
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** The flag, as React state. */
export function useGrantsTurnedOff(): boolean {
  return useSyncExternalStore(subscribe, grantsTurnedOff, grantsTurnedOff);
}

/** True for Main's 422 body that refuses a grant because unattended grants are off. */
export function isGrantsOffRefusal(body: unknown): boolean {
  return isRecord(body) && body['error'] === 'unattended_grant_refused' && body['reason'] === 'disabled';
}

/**
 * True for the desktop host's refusal of a grant because unattended grants are
 * off: the host gives that refusal its own code (`GRANTS_OFF` in
 * apps/elitea-desktop/src-tauri/src/d0/unattended.rs).
 */
export function isDesktopGrantsOff(code: string): boolean {
  return code === 'unattended_grants_off';
}
