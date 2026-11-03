/**
 * "Run to completion" of a folder (ADR-0045): on by default, kept per folder.
 * A folder with no kept choice starts on, with the default grant: each turn
 * continues by itself at the step limit and at the 512 tool-call cap, up to
 * the chain limits. Hook asks and remote confirmations stay off until the
 * person turns them on. The choice is independent of Act, Plan and Bypass.
 */
import { useState } from 'react';

import { defaultChoice, readChoice, type RunChoice } from '@/features/unattended';
import { createStorage } from '@/shared/lib/storage';

const choiceKey = (workspaceId: string): string => `desktop.workspace.${workspaceId}.runToCompletion`;

function readKept(workspaceId: string): RunChoice {
  try {
    return createStorage('local').getJSON(choiceKey(workspaceId), readChoice) ?? defaultChoice();
  } catch {
    return defaultChoice();
  }
}

function writeKept(workspaceId: string, choice: RunChoice): void {
  try {
    createStorage('local').setJSON(choiceKey(workspaceId), choice);
  } catch {
    // Storage may be unavailable: the choice lasts for this visit only.
  }
}

export interface RunToCompletion {
  choice: RunChoice;
  setChoice: (next: RunChoice) => void;
}

export function useRunToCompletion(workspaceId: string): RunToCompletion {
  const [choice, setKept] = useState(() => readKept(workspaceId));
  return {
    choice,
    setChoice: (next) => {
      setKept(next);
      writeKept(workspaceId, next);
    },
  };
}
