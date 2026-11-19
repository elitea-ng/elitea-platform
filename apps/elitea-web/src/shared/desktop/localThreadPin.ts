/**
 * The seam between the skill dialogs (`features/skills`) and the desktop host's local threads (#2183): a conversation
 * that is a local thread on this computer keeps its pins in the host, not on the server. The desktop app registers a
 * finder once at start; the dialog asks it which pin to use for the conversation it was given. Nothing is registered
 * in the web build, so the answer there is always "not local".
 */

/** Pins a project skill to one local thread; rejects with the host's refusal (`thread_skills_full`, `skill_unknown`). */
export type LocalThreadPin = (skillId: number) => Promise<void>;

/** The pin for `conversationId` when this computer ran it as a local thread, else `null`. */
export type LocalThreadPinFinder = (conversationId: string, projectId: number) => Promise<LocalThreadPin | null>;

let registered: LocalThreadPinFinder | undefined;

/** Called by the desktop app at start; `undefined` unregisters (tests). */
export function registerLocalThreadPinFinder(finder: LocalThreadPinFinder | undefined): void {
  registered = finder;
}

/** The pin for a local thread, or `null` for a conversation the server holds (and always on the web). */
export async function findLocalThreadPin(conversationId: string, projectId: number): Promise<LocalThreadPin | null> {
  return registered === undefined ? null : registered(conversationId, projectId).catch(() => null);
}
