/**
 * Unattended mode of a folder (ADR-0045): the composer's fourth mode, beside
 * Act, Plan and Bypass. Its turns run without questions (it implies bypass)
 * and under a grant that the person reads and sets in a dialog before it
 * turns on: the host then continues the run by itself, up to the chain
 * limits. The grant is kept per folder, as bypass is. Each message the person
 * sends starts a new chain with a new grant id.
 */
import { useRef, useState } from 'react';

import { defaultGrant, readGrant, type UnattendedGrantValue } from '@/features/unattended';
import { createStorage } from '@/shared/lib/storage';

const grantKey = (workspaceId: string): string => `desktop.workspace.${workspaceId}.unattended`;

function readKept(workspaceId: string): UnattendedGrantValue | null {
  try {
    return createStorage('local').getJSON(grantKey(workspaceId), readGrant);
  } catch {
    return null;
  }
}

function writeKept(workspaceId: string, grant: UnattendedGrantValue | null): void {
  try {
    const storage = createStorage('local');
    if (grant === null) storage.remove(grantKey(workspaceId));
    else storage.setJSON(grantKey(workspaceId), grant);
  } catch {
    // Storage may be unavailable: the grant lasts for this visit only.
  }
}

export interface UnattendedMode {
  /** The folder's grant while the mode is on; null when it is off. */
  grant: UnattendedGrantValue | null;
  /** Turn it on (after the person sets the grant in the dialog) or off; `turnedOn` runs once it is on. */
  request: (on: boolean, turnedOn?: () => void) => void;
  /** Open the dialog on the grant in use. */
  edit: () => void;
  /** The grant in the open dialog; null when the dialog is closed. */
  draft: UnattendedGrantValue | null;
  setDraft: (next: UnattendedGrantValue) => void;
  /** Keep the dialog's grant and turn the mode on. */
  start: () => void;
  cancel: () => void;
}

export function useUnattendedMode(workspaceId: string): UnattendedMode {
  const [grant, setGrant] = useState(() => readKept(workspaceId));
  const [draft, setDraft] = useState<UnattendedGrantValue | null>(null);
  // What runs once the dialog's Start turns the mode on (the composer leaves plan and bypass mode).
  const pending = useRef<(() => void) | undefined>(undefined);
  const keep = (next: UnattendedGrantValue | null): void => {
    setGrant(next);
    writeKept(workspaceId, next);
  };
  return {
    grant,
    request: (on, turnedOn) => {
      if (!on) {
        keep(null);
        return;
      }
      pending.current = turnedOn;
      setDraft(grant ?? defaultGrant());
    },
    edit: () => setDraft(grant ?? defaultGrant()),
    draft,
    setDraft,
    start: () => {
      if (draft === null) return;
      keep(draft);
      setDraft(null);
      pending.current?.();
      pending.current = undefined;
    },
    cancel: () => {
      pending.current = undefined;
      setDraft(null);
    },
  };
}
