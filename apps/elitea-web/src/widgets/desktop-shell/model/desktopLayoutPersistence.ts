/**
 * Pane visibility and the changes panel's width, remembered per viewer
 * (`el.desktop.layout`, swept by logout like every `el.` key). Read once
 * when the shell mounts, written on every change.
 */
import { createStorage } from '@/shared/lib/storage';

import { clampChangesWidth } from './desktopLayout.store';

export interface PersistedLayout {
  sidebarOpen: boolean;
  changesOpen: boolean;
  changesWidth: number;
}

const KEY = 'desktop.layout';

function validate(raw: unknown): PersistedLayout {
  const { sidebarOpen, changesOpen, changesWidth } = (raw ?? {}) as Record<string, unknown>;
  if (typeof sidebarOpen !== 'boolean' || typeof changesOpen !== 'boolean' || typeof changesWidth !== 'number') throw new Error('invalid');
  return { sidebarOpen, changesOpen, changesWidth: clampChangesWidth(changesWidth) };
}

export function readPersistedLayout(): PersistedLayout | null {
  try {
    return createStorage('local').getJSON(KEY, validate);
  } catch {
    return null;
  }
}

export function writePersistedLayout(layout: PersistedLayout): void {
  try {
    createStorage('local').setJSON(KEY, layout);
  } catch {
    // Not remembered.
  }
}
