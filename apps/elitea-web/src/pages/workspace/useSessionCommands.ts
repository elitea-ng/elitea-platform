/**
 * What the composer's local "/" commands do on the session screen, and the
 * state they drive: plan mode, the help / nothing-to-undo notice and the
 * undo confirmation request.
 */
import { useRef, useState, type RefObject } from 'react';

import type { WorkspaceCommandId, WorkspaceTurn } from '@/features/workspace';

import type { AgentSelection } from './useAgentSelection';

type SessionNotice = 'help' | 'nothingToUndo' | null;

export interface SessionCommands {
  planMode: boolean;
  setPlanMode: (planMode: boolean) => void;
  notice: SessionNotice;
  closeNotice: () => void;
  /** Bumped by `/undo`: the changed-files card asks for confirmation. */
  undoRequest: number;
  /** The box holding the agent select, focused by `/agent`. */
  agentRef: RefObject<HTMLDivElement | null>;
  run: (command: WorkspaceCommandId) => void;
}

export function useSessionCommands(selection: AgentSelection, turn: WorkspaceTurn, canUndo: boolean): SessionCommands {
  const [planMode, setPlanMode] = useState(false);
  const [notice, setNotice] = useState<SessionNotice>(null);
  const [undoRequest, setUndoRequest] = useState(0);
  const agentRef = useRef<HTMLDivElement>(null);

  const actions: Record<WorkspaceCommandId, () => void> = {
    new: () => {
      selection.selectConversation('');
      turn.clear();
    },
    plan: () => setPlanMode((value) => !value),
    undo: () => {
      if (canUndo) setUndoRequest((value) => value + 1);
      else setNotice('nothingToUndo');
    },
    agent: () => agentRef.current?.querySelector<HTMLElement>('[role="combobox"]')?.focus(),
    clear: () => turn.clear(),
    help: () => setNotice('help'),
  };

  return {
    planMode,
    setPlanMode,
    notice,
    closeNotice: () => setNotice(null),
    undoRequest,
    agentRef,
    run: (command) => {
      setNotice(null);
      actions[command]();
    },
  };
}
