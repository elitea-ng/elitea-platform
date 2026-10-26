/**
 * Bypass mode of a folder (IPC.md "Bypass mode"): the composer's third mode,
 * beside Act and Plan. Its turns run every local tool call that would be
 * asked without the question; the policy, the forbidden rules and the
 * person's hooks still apply. The choice is kept per folder. The first time
 * a folder turns it on, the person confirms a warning; the folder remembers
 * the confirmation.
 */
import { useRef, useState } from 'react';

import { createStorage } from '@/shared/lib/storage';

const bypassKey = (workspaceId: string): string => `desktop.workspace.${workspaceId}.bypass`;
const confirmedKey = (workspaceId: string): string => `desktop.workspace.${workspaceId}.bypassConfirmed`;

function readFlag(key: string): boolean {
  try {
    return createStorage('local').get(key) === '1';
  } catch {
    return false;
  }
}

function writeFlag(key: string, on: boolean): void {
  try {
    const storage = createStorage('local');
    if (on) storage.set(key, '1');
    else storage.remove(key);
  } catch {
    // Storage may be unavailable: the choice lasts for this visit only.
  }
}

export interface BypassMode {
  /** The folder's turns run in bypass mode. */
  on: boolean;
  /** Turn it on (after the confirmation, the first time) or off; `turnedOn` runs once it is on. */
  request: (on: boolean, turnedOn?: () => void) => void;
  /** The confirmation is open. */
  confirming: boolean;
  confirm: () => void;
  cancel: () => void;
}

export function useBypassMode(workspaceId: string): BypassMode {
  const [on, setOn] = useState(() => readFlag(bypassKey(workspaceId)));
  const [confirming, setConfirming] = useState(false);
  // What runs once a confirmed turn-on lands (the composer leaves plan mode).
  const pending = useRef<(() => void) | undefined>(undefined);
  const set = (next: boolean): void => {
    setOn(next);
    writeFlag(bypassKey(workspaceId), next);
  };
  return {
    on,
    request: (next, turnedOn) => {
      if (next && !readFlag(confirmedKey(workspaceId))) {
        pending.current = turnedOn;
        setConfirming(true);
        return;
      }
      set(next);
      if (next) turnedOn?.();
    },
    confirming,
    confirm: () => {
      writeFlag(confirmedKey(workspaceId), true);
      setConfirming(false);
      set(true);
      pending.current?.();
      pending.current = undefined;
    },
    cancel: () => {
      pending.current = undefined;
      setConfirming(false);
    },
  };
}
