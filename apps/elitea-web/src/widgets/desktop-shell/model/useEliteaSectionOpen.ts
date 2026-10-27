/**
 * Whether a sidebar section is expanded, remembered per viewer (an `el.`
 * key, swept by logout like every `el.` key): the "Elitea" section
 * (`el.desktop.eliteaOpen` — collapsed, it keeps only the page on screen, so
 * local work gets the room) and the conversations under its Chats row
 * (`el.desktop.chatsOpen`), and the "Workshop" group in it
 * (`el.desktop.workshopOpen`, closed until opened). The third element expands the section (and
 * remembers it): opening the chat expands both, so its conversations show.
 *
 * The date groups of the Chats list are disclosures too
 * (`el.desktop.chatGroupsOpen`, one flag per group): Pinned and Today are
 * open by default, Yesterday and older are closed, so the recent chats get
 * the room.
 */
import { useCallback, useState } from 'react';

import { createStorage } from '@/shared/lib/storage';

const ELITEA_KEY = 'desktop.eliteaOpen';
const CHATS_KEY = 'desktop.chatsOpen';
const WORKSHOP_KEY = 'desktop.workshopOpen';

function validate(raw: unknown): boolean {
  if (typeof raw !== 'boolean') throw new Error('invalid');
  return raw;
}

function readOpen(key: string, fallback: boolean): boolean {
  try {
    return createStorage('local').getJSON(key, validate) ?? fallback;
  } catch {
    return fallback;
  }
}

type SectionOpen = [open: boolean, toggle: () => void, expand: () => void];

function usePersistedOpen(key: string, fallback = true): SectionOpen {
  const [open, setOpen] = useState(() => readOpen(key, fallback));
  const change = useCallback(
    (decide: (current: boolean) => boolean) => {
      setOpen((current) => {
        const next = decide(current);
        if (next === current) return current;
        try {
          createStorage('local').setJSON(key, next);
        } catch {
          // Not remembered.
        }
        return next;
      });
    },
    [key],
  );
  const toggle = useCallback(() => change((current) => !current), [change]);
  const expand = useCallback(() => change(() => true), [change]);
  return [open, toggle, expand];
}

export function useEliteaSectionOpen(): SectionOpen {
  return usePersistedOpen(ELITEA_KEY);
}

export function useChatsSectionOpen(): SectionOpen {
  return usePersistedOpen(CHATS_KEY);
}

export function useWorkshopOpen(): SectionOpen {
  return usePersistedOpen(WORKSHOP_KEY, false);
}

const GROUPS_KEY = 'desktop.chatGroupsOpen';
/** The groups open by default (the listing's names, folded: `chatGroupKey`). */
const OPEN_BY_DEFAULT: ReadonlySet<string> = new Set(['pinned', 'today']);

/** A date group's storage key: the listing's name folded ("This week" → `this_week`). */
export function chatGroupKey(name: string): string {
  return name.toLowerCase().replaceAll(' ', '_');
}

function validateGroups(raw: unknown): Readonly<Record<string, boolean>> {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) throw new Error('invalid');
  return Object.fromEntries(Object.entries(raw).filter((entry): entry is [string, boolean] => typeof entry[1] === 'boolean'));
}

function readGroups(): Readonly<Record<string, boolean>> {
  try {
    return createStorage('local').getJSON(GROUPS_KEY, validateGroups) ?? {};
  } catch {
    return {};
  }
}

export interface ChatGroupsOpen {
  readonly isOpen: (key: string) => boolean;
  readonly toggle: (key: string) => void;
}

/** Each date group's open state, remembered per viewer; a group never set keeps its default. */
export function useChatGroupsOpen(): ChatGroupsOpen {
  const [stored, setStored] = useState(readGroups);
  const isOpen = useCallback((key: string) => stored[key] ?? OPEN_BY_DEFAULT.has(key), [stored]);
  const toggle = useCallback((key: string) => {
    setStored((current) => {
      const next = { ...current, [key]: !(current[key] ?? OPEN_BY_DEFAULT.has(key)) };
      try {
        createStorage('local').setJSON(GROUPS_KEY, next);
      } catch {
        // Not remembered.
      }
      return next;
    });
  }, []);
  return { isOpen, toggle };
}
