/**
 * Whether the sidebar's "Elitea" section is expanded, remembered per viewer
 * (`el.desktop.eliteaOpen`, swept by logout like every `el.` key). Collapsed,
 * it keeps only the page on screen, so local work gets the room.
 */
import { useCallback, useState } from 'react';

import { createStorage } from '@/shared/lib/storage';

const KEY = 'desktop.eliteaOpen';

function validate(raw: unknown): boolean {
  if (typeof raw !== 'boolean') throw new Error('invalid');
  return raw;
}

function readEliteaOpen(): boolean {
  try {
    return createStorage('local').getJSON(KEY, validate) ?? true;
  } catch {
    return true;
  }
}

export function useEliteaSectionOpen(): [boolean, () => void] {
  const [open, setOpen] = useState(readEliteaOpen);
  const toggle = useCallback(() => {
    setOpen((current) => {
      const next = !current;
      try {
        createStorage('local').setJSON(KEY, next);
      } catch {
        // Not remembered.
      }
      return next;
    });
  }, []);
  return [open, toggle];
}
