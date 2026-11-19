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
import { useEffect, useLayoutEffect, useRef, useSyncExternalStore } from 'react';

import { createStorage, onNamespaceCleared } from '@/shared/lib/storage';

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

/**
 * The parsed alias map, kept in memory: the composer asks for its slot on
 * every render, and a storage read plus a JSON parse per keystroke is not
 * needed for a map that changes when a conversation loads. Dropped when this
 * module writes it, when the sign-out sweep clears the namespace, and when
 * another tab writes the key (`storage`).
 */
let aliasCache: Readonly<Record<string, string>> | undefined;
const dropAliasCache = (): void => {
  aliasCache = undefined;
};
onNamespaceCleared(dropAliasCache);
if (typeof window !== 'undefined') window.addEventListener('storage', dropAliasCache);

function readAliases(): Readonly<Record<string, string>> {
  if (aliasCache !== undefined) return aliasCache;
  try {
    aliasCache = createStorage('local').getJSON(ALIASES_KEY, validateAliases) ?? {};
  } catch {
    return {};
  }
  return aliasCache;
}

const slotListeners = new Set<() => void>();
function subscribeSlots(listener: () => void): () => void {
  slotListeners.add(listener);
  return () => {
    slotListeners.delete(listener);
  };
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
  try {
    registerAlias(alias, canonical);
  } catch {
    // Storage refused: the two forms keep separate drafts.
  }
  // A composer already showing `alias` re-reads: its slot may have changed.
  for (const listener of slotListeners) listener();
}

function registerAlias(alias: string, canonical: string): void {
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
    dropAliasCache();
  } catch {
    dropAliasCache();
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
 * Text a composer held when it went away WITHOUT a draft key (the chat page
 * before its project or conversation resolves): the composer is rebuilt once
 * they do, and the new one has a key but no memory of what was typed in the
 * old. Handed over once, and only within UNKEYED_HANDOVER_MS: a rebuild is the
 * same interaction, a composer opened later is another one.
 */
const UNKEYED_HANDOVER_MS = 5_000;
let unkeyedHandover: { readonly text: string; readonly at: number } | undefined;

function takeUnkeyedHandover(): string {
  const held = unkeyedHandover;
  unkeyedHandover = undefined;
  return held !== undefined && Date.now() - held.at <= UNKEYED_HANDOVER_MS ? held.text : '';
}

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
  // Whether this composer held text since its key arrived: an input the person emptied (a send) is not a box that never had text.
  const touched = useRef(false);
  useLayoutEffect(() => {
    latest.current = { text, restore };
    if (text.trim() !== '') touched.current = true;
  });
  const previous = useRef(key);
  // The slot of `key` changes when its conversation's other id is registered (the uuid link of a draft saved under the numeric id).
  const slot = useSyncExternalStore(subscribeSlots, () => (key === undefined ? '' : slotOf(key)), () => '');
  const seen = useRef({ key, slot });
  useEffect(() => {
    const was = seen.current;
    seen.current = { key, slot };
    if (was.key !== key) touched.current = false;
    if (key === undefined || was.key !== key || was.slot === slot) return;
    // Same key, new slot: restore only into a box that never held text, never over what is being typed or was just sent.
    const saved = readDraft(key);
    if (saved !== '' && !touched.current && latest.current.text.trim() === '') {
      latest.current.restore(saved);
      latest.current = { ...latest.current, text: saved };
    }
  }, [key, slot]);
  useEffect(() => {
    const before = previous.current;
    previous.current = key;
    if (key === undefined) {
      return () => {
        if (latest.current.text.trim() !== '') unkeyedHandover = { text: latest.current.text, at: Date.now() };
      };
    }
    const saved = readDraft(key);
    const stashed = takeUnkeyedHandover();
    const handed = saved === '' && latest.current.text === '' ? stashed : '';
    if (handed !== '') {
      // A composer rebuilt as its key resolved inherits what the unkeyed one held.
      latest.current.restore(handed);
      latest.current = { ...latest.current, text: handed };
    } else if (isNewDraft(before) && before !== key && saved !== '' && saved === latest.current.text) {
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
