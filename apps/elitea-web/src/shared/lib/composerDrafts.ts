/**
 * What was typed in a composer and not sent, kept per conversation: open a
 * chat or a Local work thread, type, go to another one (or another project,
 * or quit), come back, and the text is there to finish. A composer opts in
 * with a `draftKey` (`NewChatInput`'s `content.draftKey`) naming what the text
 * belongs to — the chat's conversation, the folder's thread, or "a new chat in
 * this project" before one exists.
 *
 * Kept in this device's storage (`el.composer.drafts`, swept at sign-out like
 * every `el.` key): a draft is the person's own unsent text. The newest
 * {@link MAX_DRAFTS} are kept. An emptied input (a send, a clear) drops its
 * draft; a failed send leaves the text, and so the draft.
 */
import { useEffect, useLayoutEffect, useRef } from 'react';

import { createStorage } from '@/shared/lib/storage';

const DRAFTS_KEY = 'composer.drafts';
export const MAX_DRAFTS = 50;
/** Writes wait for a pause in typing. */
const SAVE_DELAY_MS = 300;

interface Draft {
  readonly text: string;
  readonly at: number;
}

type Drafts = Readonly<Record<string, Draft>>;

function validate(raw: unknown): Drafts {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) throw new Error('invalid');
  const drafts: Record<string, Draft> = {};
  for (const [key, value] of Object.entries(raw as Record<string, unknown>)) {
    const draft = value as Partial<Draft> | null;
    if (typeof draft?.text === 'string' && typeof draft.at === 'number') drafts[key] = { text: draft.text, at: draft.at };
  }
  return drafts;
}

function readAll(): Drafts {
  try {
    return createStorage('local').getJSON(DRAFTS_KEY, validate) ?? {};
  } catch {
    return {};
  }
}

export function readDraft(key: string): string {
  return readAll()[key]?.text ?? '';
}

export function writeDraft(key: string, text: string): void {
  const rest = Object.entries(readAll()).filter(([other]) => other !== key);
  const kept = text.trim() === '' ? rest : [...rest, [key, { text, at: Date.now() }] as const];
  const newest = kept.sort(([, a], [, b]) => b.at - a.at).slice(0, MAX_DRAFTS);
  try {
    createStorage('local').setJSON(DRAFTS_KEY, Object.fromEntries(newest));
  } catch {
    // Storage refused (quota, private mode): the draft lasts while the composer stays open.
  }
}

/**
 * A new chat became a conversation while text was in the composer (picking an
 * agent from "+" creates it): the text goes on as that conversation's draft.
 * Called by the composer's owner at the moment of creation — only it can tell
 * a creation from opening another conversation, which must not take the text.
 */
export function carryDraft(to: string, text: string): void {
  writeDraft(to, text);
}

/** The draft of a chat not created yet (`…:new`). */
const isNewDraft = (key: string | undefined): key is string => key?.endsWith(':new') === true;

/**
 * Restores `key`'s draft into the composer (`restore`) when the key arrives
 * or changes, saves the text as it is typed, and saves it once more as the
 * key changes or the composer goes, so the last keystrokes before a switch
 * are not lost to the delay.
 */
export function useComposerDraft(key: string | undefined, text: string, restore: (text: string) => void): void {
  const latest = useRef({ text, restore });
  // A layout effect: it runs before the passive cleanup below, so a send that empties the input
  // in the same render as a key change saves the emptied text, not the sent one.
  useLayoutEffect(() => {
    latest.current = { text, restore };
  });
  const previous = useRef(key);
  useEffect(() => {
    const before = previous.current;
    previous.current = key;
    if (key === undefined) return undefined;
    const saved = readDraft(key);
    if (isNewDraft(before) && before !== key && saved !== '' && saved === latest.current.text) {
      // The new chat became this conversation and its text was carried here (`carryDraft`): the new chat starts empty again.
      writeDraft(before, '');
    } else if (saved !== latest.current.text) {
      // Another conversation's text never stays behind: no draft here empties the input.
      latest.current.restore(saved);
    }
    return () => writeDraft(key, latest.current.text);
  }, [key]);
  useEffect(() => {
    if (key === undefined) return undefined;
    const timer = window.setTimeout(() => writeDraft(key, text), SAVE_DELAY_MS);
    return () => window.clearTimeout(timer);
  }, [key, text]);
}
