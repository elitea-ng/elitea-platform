/**
 * Run-dispatch slice of `./useToolkitChat.hooks.ts` (issue #93), split out
 * of that file for the same reason `./useToolkitChatSocket.hooks.ts` was:
 * to keep it under the §3.5 file-length budget.
 *
 * SSE-first: POST the run (`../../indexes/api/indexesApi.ts`'s
 * `startIndexExecution`) and hand the returned `task_id` to the caller,
 * whose `useToolkitChatSocket` then follows that execution's durable event
 * stream. The socket.io `chat_predict` emit is the FALLBACK.
 *
 * THE FALLBACK IS DECIDED TWICE, and both points matter:
 *
 *  1. Before the POST — `index_data` only. The Go handler
 *     (`services/elitea-main/internal/api/v2/indexing/start_handler.go`)
 *     rejects any other `tool_name` outright ("Only asynchronous index_data
 *     admission is supported"; every other tool "remain[s] on the current
 *     implementation until their terminal result and streaming contracts
 *     are migrated"). Attaching the contract to a plain test-tool run would
 *     buy a guaranteed 422 and a wasted round-trip on every run.
 *
 *  2. After the stream fails to connect — `runSocketFallback`. A `task_id`
 *     in the response does NOT prove the run is on the Go runtime: the
 *     legacy pylon route honours `await_response=false` and returns a
 *     `task_id` of its own, while serving no `/executions/…/events` stream
 *     at all. Nothing in the response distinguishes the two (Go answers
 *     `{task_id}` and nothing else), so the STREAM ITSELF is the
 *     discriminator: if it fails to open, the run is on legacy (or the
 *     route is gone), and the socket emit fires after all. Without this the
 *     frontend would suppress the only working transport and follow a 404.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * THE FOURTH CASE: THERE IS NO FALLBACK TRANSPORT (added 2026-09-07)
 * ─────────────────────────────────────────────────────────────────────────
 * Everything above assumes `emitOverSocket()` reaches something. On this
 * platform it usually does not.
 *
 * `shared/api/socket/client.ts:86-113` builds a NULL client whenever
 * `VITE_SOCKET_SERVER` is absent or empty — "e.g. E2E compose, offline dev" —
 * whose `emit` is documented as a "harmless no-op" and whose connection state
 * is permanently `'disconnected'`. Harmless is right for a presence indicator.
 * It is not right here: for every index tool that is NOT `index_data`
 * (`search_index_data`, `stepback_search_index`, `stepback_summary_index`)
 * this emit is the ONLY dispatch path, because the Go start handler
 * (`internal/api/v2/indexing/start_handler.go:87-90`) rejects any other
 * `tool_name` with `"Input should be 'index_data'"`. So pressing Run on the
 * index search tab called a function that did nothing, told no one, and left
 * the chat panel waiting for a reply that no server had been asked for.
 *
 * That is the whole bug: not that search is unimplemented — it genuinely is
 * not served here — but that the product presented it as working. The run is
 * still attempted (a deployment that DOES configure a socket server is
 * unchanged), and the report fires only when there is provably no transport
 * to attempt it on.
 *
 * A THIRD case, issue #310: the POST itself can answer `409 "Indexing is
 * already in progress for this index"`, naming the `task_id` of the run
 * already admitted. That is not a failure to fall back from — it is proof
 * an execution exists, on the Go runtime, right now. The old code swallowed
 * every `startIndexExecution` rejection in a bare `catch {}` and fell
 * through to the socket emit unconditionally, which STARTED A SECOND RUN on
 * top of the one the 409 was reporting. `parseIndexStartConflictTaskId`
 * (`../../indexes/lib/helpers/indexExecution.helpers.ts`) recognises only
 * that exact conflict shape; when it does, this hook ADOPTS the returned
 * task id — same as a normal successful start — and deliberately leaves
 * `pendingFallbackRef` unset, so a later stream failure can never queue a
 * socket-fallback emit for this run: retrying over socket.io would itself
 * be the duplicate run this branch exists to prevent.
 */
import { useCallback, useRef } from 'react';

import { useSocketClient, type SocketClient } from '@/shared/api/socket/client';
import { t } from '@/shared/i18n';

import { startIndexExecution } from '../../indexes/api/indexesApi';
import { IndexesToolsEnum } from '../../indexes/lib/constants/indexDetails.constants';
import { isBoundedIndexExecutionTaskId, parseIndexStartConflictTaskId } from '../../indexes/lib/helpers/indexExecution.helpers';
import type { CreatedConversation } from '../helpers/toolkitConversation.helpers';
import { findToolkitParticipant } from '../helpers/toolkitConversation.helpers';
import type { ToolkitChatLlmSettings, ToolkitChatModel, UseToolkitChatParams } from './useToolkitChat.types';

export interface UseToolkitRunDispatchParams {
  readonly projectId: string | undefined;
  readonly toolkitId: string | undefined;
  readonly selectedModel: ToolkitChatModel | null;
  readonly llmSettings: ToolkitChatLlmSettings;
  readonly buildMessagePayload: UseToolkitChatParams['buildMessagePayload'];
  /** Called with the REST `task_id` the moment it is known — the socket path only learns it from a `start_task` frame. */
  readonly onStartTask: (taskId: string | undefined) => void;
  /** Publishes the execution to follow; `undefined` means "this run is on the socket fallback". */
  readonly setExecutionId: (executionId: string | undefined) => void;
  /** Reports a run that could not be dispatched at all. Optional: a caller that does not pass it gets exactly the previous, silent behaviour. */
  readonly onError?: ((message: string) => void) | undefined;
}

export interface UseToolkitRunDispatchResult {
  readonly startToolRun: (
    currentConversation: CreatedConversation | null,
    tool: string,
    relevantInputVariables: Readonly<Record<string, unknown>>,
  ) => Promise<void>;
  /** Emit the run on socket.io after all — call when the SSE stream this run switched to fails to connect. Idempotent, and a no-op for a run that never took the SSE path. */
  readonly runSocketFallback: () => void;
}

/**
 * Reports a run that has no transport to travel on — the "fourth case" in this
 * module's header.
 *
 * Called BEFORE the emit, not instead of it: the emit stays for the
 * deployments that DO configure a socket server, and the state is read at
 * dispatch time rather than cached, so a reconnect in progress is never
 * mistaken for a missing server. Only the permanent `'disconnected'` state —
 * which is the null client's fixed value — produces the report.
 */
function reportMissingTransport(socket: SocketClient, onError: ((message: string) => void) | undefined): void {
  if (onError === undefined) return;
  if (socket.getConnectionState() !== 'disconnected') return;
  onError(
    t(
      'features.toolkits.toolkitChat.noRunTransport',
      'This run needs a live connection to the toolkit runtime, and none is configured for this deployment. Only indexing (“Index data”) can run here.',
    ),
  );
}

export function useToolkitRunDispatch(params: UseToolkitRunDispatchParams): UseToolkitRunDispatchResult {
  const { projectId, toolkitId, selectedModel, llmSettings, buildMessagePayload, onStartTask, setExecutionId, onError } = params;
  const socket = useSocketClient();

  /**
   * The socket emit this run WOULD have made, parked for as long as the run
   * is riding the SSE stream. Cleared once used and at the start of every
   * new run, so a late failure from a previous execution cannot resurrect
   * its emit.
   */
  const pendingFallbackRef = useRef<(() => void) | null>(null);

  /**
   * The reporter, held in a ref so it does not enter `startToolRun`'s
   * dependency array (§3.5 caps it at 8, and this hook is already at that
   * cap).
   *
   * This is the SAFE direction of the latest-ref pattern, not the unsafe one.
   * The closure-staleness class of bug comes from READING A STALE value at
   * call time; here the ref is rewritten on every render and read only inside
   * a synchronous, user-initiated dispatch, so what it yields is by
   * construction the newest reporter — which is exactly what a report about
   * the run happening NOW should go to. Nothing else about the run is read
   * from it.
   */
  const onErrorRef = useRef(onError);
  onErrorRef.current = onError;

  const runSocketFallback = useCallback(() => {
    const emit = pendingFallbackRef.current;
    if (!emit) return;
    pendingFallbackRef.current = null;
    setExecutionId(undefined);
    emit();
  }, [setExecutionId]);

  const startToolRun = useCallback(
    async (currentConversation: CreatedConversation | null, tool: string, relevantInputVariables: Readonly<Record<string, unknown>>) => {
      const toolkitParticipant = findToolkitParticipant(currentConversation);
      const payload = buildMessagePayload({
        conversation_uuid: currentConversation?.uuid,
        interaction_uuid: crypto.randomUUID(),
        projectId,
        selectedModel,
        participant: toolkitParticipant,
        llmSettings,
        participants: currentConversation?.participants ?? [],
      });
      const emitOverSocket = (): void => {
        socket.emit('chat_predict', { ...payload, tool_call_input: { tool_name: tool, tool_params: relevantInputVariables } });
      };
      pendingFallbackRef.current = null;

      // Decision 1: only `index_data` has a Go contract to start (see
      // header), and only with a toolkit id — `toolkit_config.toolkit_id` is
      // required and the handler 422s without a positive one.
      if (tool === IndexesToolsEnum.indexData && toolkitId !== undefined && toolkitId !== '') {
        try {
          const started = await startIndexExecution({
            projectId,
            toolkitId,
            toolParams: relevantInputVariables,
            ...(selectedModel?.name !== undefined ? { llmModel: selectedModel.name } : {}),
            llmSettings,
          });
          if (isBoundedIndexExecutionTaskId(started.task_id)) {
            // Decision 2: park the emit until the stream proves it connected.
            pendingFallbackRef.current = emitOverSocket;
            setExecutionId(started.task_id);
            onStartTask(started.task_id);
            return;
          }
        } catch (error) {
          // Decision 3 (issue #310): a 409 names a run already admitted —
          // adopt its task id instead of retrying. `pendingFallbackRef`
          // stays unset (cleared above, at the top of `startToolRun`), so a
          // later stream failure can never re-dispatch this run over
          // socket.io — see this file's header.
          const conflictTaskId = parseIndexStartConflictTaskId(error);
          if (conflictTaskId !== undefined) {
            setExecutionId(conflictTaskId);
            onStartTask(conflictTaskId);
            return;
          }
          // Every other failure: fall through to the socket path below.
        }
      }

      setExecutionId(undefined);
      reportMissingTransport(socket, onErrorRef.current);
      emitOverSocket();
    },
    [projectId, toolkitId, selectedModel, llmSettings, socket, buildMessagePayload, onStartTask, setExecutionId],
  );

  return { startToolRun, runSocketFallback };
}
