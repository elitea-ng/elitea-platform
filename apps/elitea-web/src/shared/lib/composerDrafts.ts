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

function writeAll(drafts: Drafts): void {
  const newest = Object.entries(drafts).sort(([, a], [, b]) => b.at - a.at).slice(0, MAX_DRAFTS);
  try {
    createStorage('local').setJSON(DRAFTS_KEY, Object.fromEntries(newest));
  } catch {
    // Storage refused (quota, private mode): the draft lasts while the composer stays open.
  }
}

/** The ids of one conversation (its uuid and its numeric id) name one draft: `alias key -> canonical key`. */
const ALIASES_KEY = 'composer.draftAliases';
const MAX_ALIASES = 200;

function validateAliases(raw: unknown): Readonly<Record<string, string>> {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) throw new Error('invalid');
  return Object.fromEntries(Object.entries(raw as Record<string, unknown>).filter((entry): entry is [string, string] => typeof entry[1] === 'string'));
}

function readAliases(): Readonly<Record<string, string>> {
  try {
    return createStorage('local').getJSON(ALIASES_KEY, validateAliases) ?? {};
  } catch {
    return {};
  }
}

/** The slot `key`'s draft lives in: a conversation reached by its uuid shares the draft of its numeric id. */
function slotOf(key: string): string {
  return readAliases()[key] ?? key;
}

/**
 * Says that `alias` and `canonical` are one conversation (its uuid and numeric
 * id, known once it loads): they share one draft from now on, so a deep link by
 * either form finds the text. A draft already saved under `alias` (typed while
 * the conversation loaded, or before this existed) moves to `canonical` unless
 * that one already holds text.
 */
export function rememberDraftAlias(alias: string, canonical: string): void {
  if (alias === canonical || readAliases()[alias] === canonical) return;
  const all = { ...readAll() };
  const moving = all[alias];
  if (moving !== undefined) {
    delete all[alias];
    // Both forms hold text: the newer one stays, so what was just typed is never dropped for an old draft.
    const there = all[canonical];
    if (there === undefined || there.at <= moving.at) all[canonical] = moving;
    writeAll(all);
  }
  try {
    // Re-added last, so the newest aliases are the ones the cap keeps.
    const others = Object.entries(readAliases()).filter(([key]) => key !== alias);
    createStorage('local').setJSON(ALIASES_KEY, Object.fromEntries([...others, [alias, canonical]].slice(-MAX_ALIASES)));
  } catch {
    // Storage refused: the two forms keep separate drafts.
  }
}

export function readDraft(key: string): string {
  return readAll()[slotOf(key)]?.text ?? '';
}

export function writeDraft(key: string, text: string): void {
  const slot = slotOf(key);
  const rest = Object.entries(readAll()).filter(([other]) => other !== slot);
  writeAll(Object.fromEntries(text.trim() === '' ? rest : [...rest, [slot, { text, at: Date.now() }] as const]));
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
 * or changes (text typed BEFORE the key arrived is kept, not replaced), saves the text as it is typed, and saves it once more as the
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
    } else if (saved !== latest.current.text && !(before === undefined && latest.current.text.trim() !== '')) {
      // Another conversation's text never stays behind: no draft here empties the input.
      // Text typed while there was no key at all (the chat page before its project resolves) belongs to
      // nobody else: it is kept and becomes this key's draft, never discarded for the saved one.
      latest.current.restore(saved);
      // The input holds this key's text from now on, even before that render lands: a composer
      // unmounted first (Create remounts the chat) must not save the text it is leaving under this key.
      latest.current = { ...latest.current, text: saved };
    }
    return () => writeDraft(key, latest.current.text);
  }, [key]);
  useEffect(() => {
    if (key === undefined) return undefined;
    const timer = window.setTimeout(() => writeDraft(key, latest.current.text), SAVE_DELAY_MS);
    return () => window.clearTimeout(timer);
  }, [key, text]);
}
