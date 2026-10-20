/**
 * Whether a sidebar section is expanded, remembered per viewer (an `el.`
 * key, swept by logout like every `el.` key): the "Elitea" section
 * (`el.desktop.eliteaOpen` — collapsed, it keeps only the page on screen, so
 * local work gets the room) and the conversations under its Chats row
 * (`el.desktop.chatsOpen`). The third element expands the section (and
 * remembers it): opening the chat expands both, so its conversations show.
 */
import { useCallback, useState } from 'react';

import { createStorage } from '@/shared/lib/storage';

const ELITEA_KEY = 'desktop.eliteaOpen';
const CHATS_KEY = 'desktop.chatsOpen';

function validate(raw: unknown): boolean {
  if (typeof raw !== 'boolean') throw new Error('invalid');
  return raw;
}

function readOpen(key: string): boolean {
  try {
    return createStorage('local').getJSON(key, validate) ?? true;
  } catch {
    return true;
  }
}

type SectionOpen = [open: boolean, toggle: () => void, expand: () => void];

function usePersistedOpen(key: string): SectionOpen {
  const [open, setOpen] = useState(() => readOpen(key));
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
