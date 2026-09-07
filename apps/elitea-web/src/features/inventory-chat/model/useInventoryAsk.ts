/**
 * Asking the knowledge graph a question.
 *
 * ONE TOOL, `investigate`, AND ONLY ON THE SEARCH FAMILY. The descriptor
 * declares it on `inventory_search` and nowhere else: "An AI agent will search
 * the graph, explore relationships, and provide a comprehensive answer with
 * citations." Sending it to the `inventory` family is an unknown tool, refused
 * as invalid input — which reads on screen as a broken chat rather than as the
 * wrong address.
 *
 * IT IS A WATCHED RUN, not a read, for the reason the ingestion is: the agent
 * plans, reads the graph and writes, and it streams what it is doing down
 * `custom_events` as it goes. Those events are READ-ONCE — the poll that
 * carries a line is the only poll that will — so awaiting the answer and
 * ignoring the polls throws the whole visible reasoning away and leaves a
 * spinner for as long as the agent takes.
 *
 * THE ANSWER IS TEXT AND THE CITATIONS ARE IDS. `investigate` answers a
 * document with `answer` and `entities`; the ids are what makes the answer
 * checkable — the reader clicks one and lands on the entity the agent used.
 */
import { useCallback, useRef, useState } from 'react';

import {
  INVENTORY_SEARCH_FAMILY,
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

/** How often a running question is polled. */
const ASK_POLL_INTERVAL_MS = 1000;

/** The tool the question is sent to. */
const ASK_TOOL = 'investigate';

/** One exchange, as the transcript holds it. */
export interface AskTurn {
  readonly question: string;
  readonly answer: string;
  /** The entity ids the agent said it used. Clickable in the panel. */
  readonly entities: readonly string[];
}

/** What the ask panel renders and drives. */
export interface InventoryAskController {
  readonly turns: readonly AskTurn[];
  /** The question being answered, or null. */
  readonly pendingQuestion: string | null;
  /** What the agent has said it is doing, for the question in flight. */
  readonly steps: readonly string[];
  readonly error: string | null;
  readonly ask: (question: string) => void;
  readonly stop: () => void;
  readonly clear: () => void;
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

/** The ids the agent cited, as text, with the unusable ones dropped. */
function citedEntities(document: Record<string, unknown> | undefined): readonly string[] {
  const raw = document?.['entities'];
  if (!Array.isArray(raw)) return [];
  const ids: string[] = [];
  for (const entry of raw as readonly unknown[]) {
    const id = typeof entry === 'string' ? entry : '';
    if (id !== '' && !ids.includes(id)) ids.push(id);
  }
  return ids;
}

export function useInventoryAsk(target: InventoryTarget): InventoryAskController {
  const [turns, setTurns] = useState<readonly AskTurn[]>([]);
  const [pendingQuestion, setPendingQuestion] = useState<string | null>(null);
  const [invocationId, setInvocationId] = useState<string | null>(null);
  const [steps, setSteps] = useState<readonly string[]>([]);
  const [error, setError] = useState<string | null>(null);
  // The question is held in a ref as well as in state: the poll handler needs
  // it when the answer lands, and reading it from state there would capture
  // whatever it was when the handler was created.
  const questionRef = useRef('');
  const settledRef = useRef(false);
  // A Stop pressed BEFORE the invoke answers has no id to cancel. Without this
  // the press did nothing and the agent went on running — a window of a second
  // or two on a control whose whole purpose is to be pressed immediately.
  const stopRequestedRef = useRef(false);
  // The id is kept in a REF as well as in state, and `stop` reads the ref.
  // State reaches a callback only through the render that follows it, so a
  // Stop pressed between the invoke answering and React re-rendering would
  // read `null` and do nothing — a race a user hits by pressing the button as
  // soon as it appears, which is exactly when they press it.
  const invocationIdRef = useRef<string | null>(null);

  const ask = useCallback(
    (question: string) => {
      const text = question.trim();
      // A second question while one is running would start an invocation whose
      // events interleave with the first one's on a screen that shows one
      // transcript. Refused silently: the composer is disabled while a
      // question runs, so this is the belt to that brace.
      if (text === '' || invocationId !== null) return;
      questionRef.current = text;
      settledRef.current = false;
      stopRequestedRef.current = false;
      setPendingQuestion(text);
      setSteps([]);
      setError(null);
      void inventoryInvocations
        .start(target, INVENTORY_SEARCH_FAMILY, ASK_TOOL, {
          question: text,
          output_format: 'json',
        })
        .then((id) => {
          invocationIdRef.current = id;
          setInvocationId(id);
          // The user pressed Stop while this invoke was in flight. Cancel the
          // run the moment it has a name, rather than starting to poll one
          // nobody is waiting for.
          if (stopRequestedRef.current) {
            void inventoryInvocations.cancel(target, INVENTORY_SEARCH_FAMILY, ASK_TOOL, id);
          }
        })
        .catch((reason: unknown) => {
          setPendingQuestion(null);
          setError(reason instanceof Error ? reason.message : 'The question could not be asked.');
        });
    },
    [invocationId, target],
  );

  const stop = useCallback(() => {
    stopRequestedRef.current = true;
    const id = invocationIdRef.current;
    if (id === null) return;
    void inventoryInvocations.cancel(target, INVENTORY_SEARCH_FAMILY, ASK_TOOL, id);
  }, [target]);

  const clear = useCallback(() => {
    // The RUN is not cancelled here: clearing the transcript is a view action,
    // and stopping an invocation the user did not ask to stop would lose an
    // answer they are still waiting for.
    setTurns([]);
    setError(null);
  }, []);

  const handlePoll = useCallback((poll: InvocationPoll | undefined) => {
    const lines = drainEventMessages(poll).map(progressText).filter((line) => line !== '');
    if (lines.length > 0) setSteps((current) => [...current, ...lines]);

    const outcome = terminalOutcome(poll, 'The question ended without an answer.');
    if (outcome === null || settledRef.current) return;
    settledRef.current = true;
    invocationIdRef.current = null;
    setInvocationId(null);
    setPendingQuestion(null);

    if (outcome.kind === 'failed') {
      setError(inventoryDocuments.errorText(outcome.message));
      return;
    }
    const document = inventoryDocuments.document(outcome.result);
    const answer = typeof document?.['answer'] === 'string'
      ? document['answer']
      // A provider that answered text rather than the document still answered.
      // Falling back to the peeled message keeps a working engine usable
      // instead of showing an empty bubble.
      : inventoryDocuments.text(outcome.result);
    setTurns((current) => [
      ...current,
      { question: questionRef.current, answer, entities: citedEntities(document) },
    ]);
  }, []);

  useInvocationPoll(invocationId, {
    poll: (id) => inventoryInvocations.poll(target, INVENTORY_SEARCH_FAMILY, ASK_TOOL, id),
    onPoll: handlePoll,
    intervalMs: ASK_POLL_INTERVAL_MS,
  });

  return { turns, pendingQuestion, steps, error, ask, stop, clear };
}
