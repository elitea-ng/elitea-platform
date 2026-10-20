/**
 * Whether a sidebar section is expanded, remembered per viewer (an `el.`
 * key, swept by logout like every `el.` key): the "Elitea" section
 * (`el.desktop.eliteaOpen` — collapsed, it keeps only the page on screen, so
 * local work gets the room) and the conversations under its Chats row
 * (`el.desktop.chatsOpen`).
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

function usePersistedOpen(key: string): [boolean, () => void] {
  const [open, setOpen] = useState(() => readOpen(key));
  const toggle = useCallback(() => {
    setOpen((current) => {
      const next = !current;
      try {
        createStorage('local').setJSON(key, next);
      } catch {
        // Not remembered.
      }
      return next;
    });
  }, [key]);
  return [open, toggle];
}

export function useEliteaSectionOpen(): [boolean, () => void] {
  return usePersistedOpen(ELITEA_KEY);
}

export function useChatsSectionOpen(): [boolean, () => void] {
  return usePersistedOpen(CHATS_KEY);
}
