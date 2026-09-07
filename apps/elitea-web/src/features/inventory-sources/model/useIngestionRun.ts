/**
 * Running one ingestion, and showing what it is doing while it runs.
 *
 * IT IS NOT A READ, so it does not go through `useInventoryTool`. Three things
 * separate it:
 *
 *  - It takes MINUTES, not seconds. A promise that only settles at the end
 *    gives the user a spinner and no evidence anything is happening; the
 *    provider streams six progress lines ("Reading the source files",
 *    "Extracting entities", …) and they are the evidence.
 *  - Those lines arrive in `custom_events`, which are READ-ONCE: the poll that
 *    carries a line is the only poll that ever will. A caller that awaits the
 *    answer and ignores the polls throws every line away.
 *  - It must be STOPPABLE. `DELETE …/invocations/{id}` is the only way to end
 *    a run that is reading the wrong branch of a large repository, and the
 *    facade answers the next poll with `Stopped`.
 *
 * WHAT IT DOES NOT DO IS POLL AFTERWARDS. When the run settles, the caller is
 * told once, and the caller invalidates the reads. Refetching from in here
 * would make this hook own the freshness of screens it cannot see.
 */
import { useCallback, useRef, useState } from 'react';

import {
  INVENTORY_FAMILY,
  inventoryDocuments,
  inventoryInvocations,
  type InventoryTarget,
} from '@/entities/inventory';
import {
  drainEventMessages,
  terminalOutcome,
  useInvocationPoll,
  type InvocationPoll,
} from '@/entities/provider-run';

/** How often a running ingestion is polled. Slower than a read: it is minutes long. */
const INGESTION_POLL_INTERVAL_MS = 2000;

/**
 * One object an ingestion landed in the bucket.
 *
 * The CHECKPOINT is why this is surfaced: `.ingestion-checkpoint-<source>.json`
 * is written per source and is the only record that a run got far enough to
 * be resumable. It is named in the terminal body and nowhere else a screen can
 * reach, so a panel that ignores the artifacts cannot report it at all.
 */
interface IngestionArtifact {
  readonly name: string;
  readonly objectType: string;
}

/** What the panel shows about the run it started. */
export interface IngestionRun {
  /** The source being ingested, or null when nothing is running. */
  readonly runningSourceId: string | null;
  /** The progress lines the provider has streamed, in order. */
  readonly steps: readonly string[];
  /** The provider's own sentence for a run that failed or was stopped. */
  readonly error: string | null;
  /** The provider's own sentence for a run that finished. */
  readonly summary: string | null;
  /** What the finished run wrote: the graph, the source status, the checkpoint. */
  readonly artifacts: readonly IngestionArtifact[];
  readonly start: (sourceToolkitId: string, fullRebuild: boolean) => void;
  readonly stop: () => void;
}

function progressText(message: unknown): string {
  if (typeof message === 'string') return message;
  if (typeof message === 'object' && message !== null) {
    const record = message as Record<string, unknown>;
    for (const key of ['message', 'text', 'content']) {
      const value = record[key];
      if (typeof value === 'string' && value !== '') return value;
    }
  }
  return '';
}

/**
 * @param onSettled called once when a run ends, however it ends. The caller
 * invalidates its reads there — the graph, the statuses and the statistics all
 * changed, and only the caller knows which of them are on screen.
 */
export function useIngestionRun(
  target: InventoryTarget,
  onSettled: () => void,
): IngestionRun {
  const [invocationId, setInvocationId] = useState<string | null>(null);
  const [runningSourceId, setRunningSourceId] = useState<string | null>(null);
  const [steps, setSteps] = useState<readonly string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [summary, setSummary] = useState<string | null>(null);
  const [artifacts, setArtifacts] = useState<readonly IngestionArtifact[]>([]);
  // The tool name is held rather than recomputed: poll and cancel address the
  // invocation by the tool it was started for, and a mismatch is a 404 that
  // reads as "the run vanished".
  const toolRef = useRef('run_ingestion');
  const settledRef = useRef(false);
  // A Stop pressed BEFORE the invoke answers has no id to cancel. The window is
  // small and the cost is not: the run it fails to stop reads a whole
  // repository, and the user has already been told it is running.
  const stopRequestedRef = useRef(false);
  // The id is kept in a REF as well as in state, for the reason the ask
  // controller records: state reaches a callback only through the render that
  // follows it, so a Stop pressed between the invoke answering and React
  // re-rendering would read `null` and leave the run going.
  const invocationIdRef = useRef<string | null>(null);

  const start = useCallback(
    (sourceToolkitId: string, fullRebuild: boolean) => {
      if (sourceToolkitId === '') return;
      setSteps([]);
      setError(null);
      setSummary(null);
      setArtifacts([]);
      setRunningSourceId(sourceToolkitId);
      settledRef.current = false;
      stopRequestedRef.current = false;
      void inventoryInvocations
        .start(target, INVENTORY_FAMILY, toolRef.current, {
          toolkit_id: sourceToolkitId,
          // Sent only when true. The provider's merge takes a tool argument
          // over a configured one ONLY when it is truthy, so `false` would be
          // ignored anyway — and sending it invites the reader of this call to
          // believe it turns a configured rebuild off.
          ...(fullRebuild ? { full_rebuild: true } : {}),
        })
        .then((id) => {
          invocationIdRef.current = id;
          setInvocationId(id);
          if (stopRequestedRef.current) {
            void inventoryInvocations.cancel(target, INVENTORY_FAMILY, toolRef.current, id);
          }
        })
        .catch((reason: unknown) => {
          setRunningSourceId(null);
          setError(reason instanceof Error ? reason.message : 'The ingestion could not be started.');
        });
    },
    [target],
  );

  const stop = useCallback(() => {
    stopRequestedRef.current = true;
    const id = invocationIdRef.current;
    if (id === null) return;
    // The state is NOT cleared here. The provider answers the next poll with
    // `Stopped`, and that poll is what settles the run — clearing now would
    // leave the poller running against an invocation the screen has forgotten.
    void inventoryInvocations.cancel(target, INVENTORY_FAMILY, toolRef.current, id);
  }, [target]);

  const handlePoll = useCallback(
    (poll: InvocationPoll | undefined) => {
      const lines = drainEventMessages(poll).map(progressText).filter((line) => line !== '');
      if (lines.length > 0) setSteps((current) => [...current, ...lines]);

      const outcome = terminalOutcome(poll, 'The ingestion ended without saying why.');
      if (outcome === null) return;
      if (settledRef.current) return;
      settledRef.current = true;
      invocationIdRef.current = null;
      setInvocationId(null);
      setRunningSourceId(null);
      if (outcome.kind === 'failed') {
        // PEELED, like a success. A refusal carries the SAME envelope:
        // `spi.ToolError` marshals `[]ResultObject{Message(text)}` into the
        // very `result` field a completed run uses
        // (services/elitea-subapp-host/internal/spi/errors.go:130-147), so the
        // "message" a terminal poll hands back is a JSON array and not a
        // sentence. Showing it unpeeled put
        // `[{"object_type":"message","data":"Run_ingestion failed: …"}]` in
        // the banner — the failure reported, the reason unreadable. Found by
        // INV-009 against a stack whose facade mounts without source
        // expansion, which is the one thing that makes this path run at all.
        setError(inventoryDocuments.errorText(outcome.message));
      } else {
        // `outcome.result` is the SPI ENVELOPE — a JSON array of result
        // objects, not the sentence. Showing it unpeeled puts a wall of
        // escaped JSON where the summary belongs, which is what the first
        // version of this hook did.
        setSummary(inventoryDocuments.text(outcome.result));
        setArtifacts(inventoryDocuments.artifacts(outcome.result));
      }
      onSettled();
    },
    [onSettled],
  );

  useInvocationPoll(invocationId, {
    poll: (id) => inventoryInvocations.poll(target, INVENTORY_FAMILY, toolRef.current, id),
    onPoll: handlePoll,
    intervalMs: INGESTION_POLL_INTERVAL_MS,
  });

  return { runningSourceId, steps, error, summary, artifacts, start, stop };
}
