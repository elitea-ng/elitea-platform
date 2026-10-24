/**
 * What the composer's local "/" commands do on the session screen, and the
 * state they drive: plan mode, the help / nothing-to-undo notice and the
 * undo confirmation request. `/retry` and `/steps` need the composer's send
 * and the folder: the composer runs them and shows their notices here.
 */
import { useRef, useState, type RefObject } from 'react';

import type { WorkspaceCommandId, WorkspaceTurn } from '@/features/workspace';

import type { AgentSelection } from './useAgentSelection';

type SessionNotice = 'help' | 'nothingToUndo' | 'nothingToRetry' | 'compactNext' | 'modelFixed' | 'steps' | null;

export interface SessionCommands {
  planMode: boolean;
  setPlanMode: (planMode: boolean) => void;
  notice: SessionNotice;
  closeNotice: () => void;
  /** What the `steps` notice says (the step limit, or why it was not set). */
  stepsText: string;
  /** Show the `steps` notice with `text`. */
  showSteps: (text: string) => void;
  /** `/retry` found no message to send again. */
  showNothingToRetry: () => void;
  /** Bumped by `/undo`: the changed-files card asks for confirmation. */
  undoRequest: number;
  /** The box holding the agent picker; `/agent` opens it. */
  agentRef: RefObject<HTMLDivElement | null>;
  /** The box holding the model chip; `/model` opens it (no chip: the agent's model is fixed here). */
  modelRef: RefObject<HTMLDivElement | null>;
  /** `/compact` was asked for: the next message's turn compacts the context first. */
  compactNext: boolean;
  /** The turn that compacts started. */
  compactSent: () => void;
  run: (command: WorkspaceCommandId) => void;
}

export function useSessionCommands(selection: AgentSelection, turn: WorkspaceTurn, canUndo: boolean): SessionCommands {
  const [planMode, setPlanMode] = useState(false);
  // An accepted plan left plan mode on the host (`plan_accepted`): the switch follows, once per accept.
  const accepted = turn.view.planAccepted;
  const [followed, setFollowed] = useState<string | undefined>(undefined);
  if (accepted !== undefined && accepted !== followed) {
    setFollowed(accepted);
    setPlanMode(false);
  }
  const [notice, setNotice] = useState<SessionNotice>(null);
  const [stepsText, setStepsText] = useState('');
  const [undoRequest, setUndoRequest] = useState(0);
  const [compactNext, setCompactNext] = useState(false);
  const agentRef = useRef<HTMLDivElement>(null);
  const modelRef = useRef<HTMLDivElement>(null);

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
    // The composer runs these two (it holds the send and the folder); here only when it does not.
    retry: () => setNotice('nothingToRetry'),
    steps: () => undefined,
    agent: () => agentRef.current?.querySelector<HTMLElement>('button')?.click(),
    model: () => {
      const chip = modelRef.current?.querySelector<HTMLElement>('button');
      if (chip === undefined || chip === null) setNotice('modelFixed');
      else chip.click();
    },
    clear: () => turn.clear(),
    compact: () => {
      setCompactNext(true);
      setNotice('compactNext');
    },
    help: () => setNotice('help'),
  };

  return {
    planMode,
    setPlanMode,
    notice,
    closeNotice: () => setNotice(null),
    stepsText,
    showSteps: (text) => {
      setStepsText(text);
      setNotice('steps');
    },
    showNothingToRetry: () => setNotice('nothingToRetry'),
    undoRequest,
    agentRef,
    modelRef,
    compactNext,
    compactSent: () => {
      setCompactNext(false);
      setNotice((current) => (current === 'compactNext' ? null : current));
    },
    run: (command) => {
      setNotice(null);
      actions[command]();
    },
  };
}
