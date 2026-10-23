/**
 * Pane visibility and the inspector's layout per kind of page (its groups,
 * tabs, split and width), remembered per viewer (`el.desktop.layout`, swept
 * by logout like every `el.` key). Read once when the shell mounts, written
 * on every change.
 *
 * A layout stored before the inspector had groups (`changesWidth`, one
 * width for the Local work panel) reads back as the default layout with that
 * width.
 */
import type { InspectorPage } from '@/shared/desktop/inspectorSlot';
import { createStorage } from '@/shared/lib/storage';

import { clampInspectorWidth, parseInspectorLayout, type InspectorLayout } from '../lib/inspectorLayout';
import { DEFAULT_INSPECTOR } from './desktopLayout.store';

export interface PersistedLayout {
  sidebarOpen: boolean;
  changesOpen: boolean;
  inspector: Readonly<Record<InspectorPage, InspectorLayout>>;
}

const KEY = 'desktop.layout';

function validate(raw: unknown): PersistedLayout {
  const { sidebarOpen, changesOpen, changesWidth, inspector } = (raw ?? {}) as Record<string, unknown>;
  if (typeof sidebarOpen !== 'boolean' || typeof changesOpen !== 'boolean') throw new Error('invalid');
  const stored = (inspector ?? {}) as Record<string, unknown>;
  const local = parseInspectorLayout(stored['local'], 'local') ?? {
    ...DEFAULT_INSPECTOR.local,
    ...(typeof changesWidth === 'number' ? { width: clampInspectorWidth(changesWidth) } : {}),
  };
  const chat = parseInspectorLayout(stored['chat'], 'chat') ?? DEFAULT_INSPECTOR.chat;
  return { sidebarOpen, changesOpen, inspector: { local, chat } };
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
