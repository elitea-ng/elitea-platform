/**
 * One synchronous "Run tool" press, and what it settled on.
 *
 * `../../api/toolkitTestRun.ts`'s `testToolkitTool` never rejects — it folds
 * every branch of `POST /elitea_core/test_tool/prompt_lib/{projectId}/{toolId}`
 * into one closed `TestToolkitToolOutcome` union — so this hook holds a single
 * outcome value and an `isRunning` flag, and has no error channel of its own.
 * A `try`/`catch` here would be dead code by construction.
 *
 * WHY A HOOK AND NOT `useMutation`. React Query's mutation state is keyed by
 * nothing and reset by no one; this pane must clear the previous result the
 * moment the picked tool changes, or a failure from the tool you just stopped
 * looking at stays on screen under the new tool's arguments. `reset()` is that
 * clearing, and it is called from the tool picker.
 *
 * THE LAST PRESS WINS. Two presses in flight settle in whatever order the
 * server answers, and rendering the slower one would show a result the user did
 * not ask for last. A monotonically increasing token records which press is
 * current; an outcome that arrives for an older one is dropped.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import type { TestToolkitToolOutcome } from '../../api/toolkitTestRun';
import { testToolkitTool } from '../../api/toolkitTestRun';

export interface UseToolkitTestToolRunParams {
  readonly projectId: string | number | undefined;
  readonly toolkitId: string | number | undefined;
}

export interface UseToolkitTestToolRunResult {
  /** The settled outcome of the most recent press, or `undefined` before the first one. */
  readonly outcome: TestToolkitToolOutcome | undefined;
  readonly isRunning: boolean;
  /** Runs one tool. Resolves when the outcome has settled (or been superseded). */
  readonly run: (toolName: string, toolParams: Readonly<Record<string, unknown>>) => Promise<void>;
  /** Drops the outcome on screen. Call it when the thing the outcome was about changes. */
  readonly reset: () => void;
  readonly authorize: (reference: string) => Promise<void>;
  readonly skip: () => void;
}

export function useToolkitTestToolRun({ projectId, toolkitId }: UseToolkitTestToolRunParams): UseToolkitTestToolRunResult {
  const [outcome, setOutcome] = useState<TestToolkitToolOutcome | undefined>(undefined);
  const [isRunning, setIsRunning] = useState(false);

  /** The press whose answer this component still wants. */
  const currentPressRef = useRef(0);
  const pendingRef = useRef<{ toolName: string; toolParams: Readonly<Record<string, unknown>>; press: number } | undefined>(undefined);
  /** False after unmount, so a late answer never sets state on a dead component. */
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const reset = useCallback(() => {
    // The press counter moves too: a run in flight when the tool changes must
    // not land on the new tool's empty result panel.
    currentPressRef.current += 1;
    pendingRef.current = undefined;
    setOutcome(undefined);
    setIsRunning(false);
  }, []);

  const run = useCallback(
    async (toolName: string, toolParams: Readonly<Record<string, unknown>>): Promise<void> => {
      currentPressRef.current += 1;
      const press = currentPressRef.current;
      setOutcome(undefined);
      setIsRunning(true);

      const savedParams = structuredClone(toolParams);
      pendingRef.current = undefined;
      const settled = await testToolkitTool({ projectId, toolkitId, toolName, toolParams: savedParams });

      if (!mountedRef.current || press !== currentPressRef.current) return;
      if (settled.kind === 'authorizationRequired') pendingRef.current = { toolName, toolParams: savedParams, press };
      setOutcome(settled);
      setIsRunning(false);
    },
    [projectId, toolkitId],
  );

  useEffect(() => { reset(); }, [projectId, toolkitId, reset]);
  const authorize = useCallback(async (reference: string) => {
    const pending = pendingRef.current;
    if (!pending || !mountedRef.current || pending.press !== currentPressRef.current || !/^[A-Za-z0-9_-]{43}$/.test(reference)) return;
    pendingRef.current = undefined;
    setIsRunning(true);
    const settled = await testToolkitTool({ projectId, toolkitId, toolName: pending.toolName, toolParams: pending.toolParams, authorizationReference: reference });
    if (!mountedRef.current || pending.press !== currentPressRef.current) return;
    setOutcome(settled);
    setIsRunning(false);
    if (settled.kind === 'authorizationRequired') pendingRef.current = pending;
  }, [projectId, toolkitId]);
  const skip = useCallback(() => { reset(); setOutcome({ kind: 'skipped' }); }, [reset]);
  return { outcome, isRunning, run, reset, authorize, skip };
}
