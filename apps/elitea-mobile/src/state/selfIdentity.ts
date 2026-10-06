import { useQuery } from '@tanstack/react-query';
import { useSyncExternalStore } from 'react';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import { log } from '@/lib/log';
import { isEndingSession, registerWipeHook } from '@/security/endSession';
import { SECURE_KEYS, getSecureItem, setSecureItem } from '@/security/secureStorage';
import { getSession, useSession } from '@/state/session';

/**
 * Who "me" is: the signed-in user's USER id, which tells my messages from
 * other people's in a shared conversation.
 *
 * It is read from the server's "who am I" (`getCurrentAuthor`, client
 * contract 1.2; the route itself is older, so every supported server answers
 * it) once per session, and cached in secure storage for the workspace, so a
 * launch offline still knows it. The cached value is part of the secure
 * registry and the in-memory copy has a wipe hook: both go with the session.
 * Only the ids are kept (the user id and, for the project switcher, the
 * personal project id); the profile's name, email and avatar are not stored.
 */

let self: { origin: string; userId: number; personalProjectId: number | null } | null = null;
const listeners = new Set<() => void>();

function emit() {
  for (const listener of [...listeners]) listener();
}

/** The caller's user id for the connected workspace, or null while unknown. */
export function getSelfUserId(): number | null {
  const origin = getSession().serverOrigin;
  return self !== null && origin !== null && self.origin === origin ? self.userId : null;
}

export function subscribeSelfIdentity(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function useSelfUserId(): number | null {
  // The origin is read so a workspace change re-renders with the right answer.
  useSession((s) => s.serverOrigin);
  return useSyncExternalStore(subscribeSelfIdentity, getSelfUserId, getSelfUserId);
}

/**
 * The caller's personal project in the connected workspace
 * (`SocialAuthorProfile.personal_project_id`, contract 1.2), or null while
 * unknown or still being provisioned.
 */
export function getPersonalProjectId(): number | null {
  const origin = getSession().serverOrigin;
  return self !== null && origin !== null && self.origin === origin ? self.personalProjectId : null;
}

export function usePersonalProjectId(): number | null {
  useSession((s) => s.serverOrigin);
  return useSyncExternalStore(subscribeSelfIdentity, getPersonalProjectId, getPersonalProjectId);
}

/** A user id as the server writes it in `SocialAuthorProfile.id` (a decimal string). */
export function parseUserId(value: unknown): number | null {
  const text = typeof value === 'number' ? String(value) : value;
  if (typeof text !== 'string' || !/^[1-9]\d{0,15}$/.test(text)) return null;
  const id = Number(text);
  return Number.isSafeInteger(id) ? id : null;
}

/** The session is being wiped: nothing may be (re)stored for it. */
function sessionEnding(): boolean {
  return getSession().ending || isEndingSession();
}

function setSelf(origin: string, userId: number, personalProjectId: number | null): void {
  if (self?.origin === origin && self.userId === userId && self.personalProjectId === personalProjectId) return;
  self = { origin, userId, personalProjectId };
  emit();
}

const PersistedSchema = z.object({
  origin: z.string().min(1),
  userId: z.number().int().positive(),
  // Added with the project switcher; absent in what earlier builds stored.
  personalProjectId: z.number().int().positive().nullable().optional(),
});

async function persist(origin: string, userId: number, personalProjectId: number | null): Promise<void> {
  // A write landing after endSession's secure delete would outlive the wipe.
  if (sessionEnding()) return;
  try {
    await setSecureItem(SECURE_KEYS.selfUser, JSON.stringify({ origin, userId, personalProjectId }));
  } catch (error) {
    log.info('self identity not persisted', { error });
  }
}

/** Launch: restores the cached id when it was stored for `origin`. Never throws. */
export async function restoreSelfIdentity(origin: string): Promise<void> {
  let raw: string | null;
  try {
    raw = await getSecureItem(SECURE_KEYS.selfUser);
  } catch {
    return;
  }
  if (raw === null) return;
  try {
    const parsed = PersistedSchema.parse(JSON.parse(raw));
    if (parsed.origin !== origin || parseUserId(parsed.userId) === null) return;
    setSelf(parsed.origin, parsed.userId, parsed.personalProjectId ?? null);
  } catch {
    log.info('cached self identity ignored');
  }
}

/**
 * Reads "who am I" and records the id. Resolves to the id, or null when the
 * answer has none; a failed read rejects (the cached id, if any, stays).
 */
export async function fetchSelfIdentity(client: ApiClient = sessionApiClient(), signal?: AbortSignal): Promise<number | null> {
  const origin = getSession().serverOrigin;
  const profile = await unwrap(client.GET('/social/author', { signal }));
  const userId = parseUserId(profile?.id);
  // "" while the personal project is still being provisioned.
  const personalProjectId = parseUserId(profile?.personal_project_id);
  // Workspace changed (or wiped, or being wiped) while reading: drop it.
  if (userId === null || origin === null || getSession().serverOrigin !== origin || sessionEnding()) return userId;
  const changed = self?.origin !== origin || self.userId !== userId || self.personalProjectId !== personalProjectId;
  setSelf(origin, userId, personalProjectId);
  if (changed) await persist(origin, userId, personalProjectId);
  return userId;
}

/** Reads "who am I" once per signed-in session (the app shell mounts it). */
export function useSelfIdentityRefresh(): void {
  const origin = useSession((s) => s.serverOrigin);
  const signedIn = useSession((s) => s.credential !== null && !s.ending);
  useQuery({
    queryKey: ['self', origin],
    enabled: signedIn && origin !== null,
    staleTime: Infinity,
    gcTime: Infinity,
    retry: 2,
    queryFn: ({ signal }) => fetchSelfIdentity(sessionApiClient(), signal),
  });
}

export function resetSelfIdentity(): void {
  self = null;
  emit();
}

registerWipeHook('self_identity', resetSelfIdentity);
