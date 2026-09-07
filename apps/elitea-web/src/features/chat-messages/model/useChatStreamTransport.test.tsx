/**
 * useChatStreamTransport.test.tsx — the transport swap (issue #93, Surface B).
 *
 * The frames replayed here are the ones a live standalone stack emitted,
 * captured while `deploy/scripts/chat-smoke.py` ran against it. The end-to-end
 * test is therefore evidence that a real recorded run renders through the real
 * SSE seam — not that a hand-written frame satisfies a hand-written reducer.
 */
import { act, render, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  configureGeneratedClient,
  resetGeneratedClient,
} from "@/shared/api/generated/mutator";
import { resetConfigForTests } from "@/shared/config/get-config";
import {
  installTestEventSource,
  type TestEventSourceRegistry,
} from "@/shared/api/sse/testing";
import { server } from "@/test/setup";

import {
  useChatStreamTransport,
  type UseChatStreamTransportResult,
} from "./useChatStreamTransport";
import type { ChatMessage } from "../lib/convertMessagesToChatHistory";

const BASE = "/api/v2";
const EVENTS_URL = "/api/v2/executions/7/exec-1/events";
const MESSAGE_ID = "63c6d989-2860-5d68-9e3e-3587c63350d3";
const QUESTION_ID = "62e87daa-1f18-4b82-9d70-69ed141c7d1f";
const RESPONSE_MESSAGE_ID = "e3f5b0f2-8a3c-4d9a-9a1e-0c2b7f5d1a44";

const globals = globalThis as unknown as Record<string, unknown>;
let registry: TestEventSourceRegistry;

function pendingAssistant(): ChatMessage {
  return {
    id: MESSAGE_ID,
    role: "assistant",
    name: "Agent",
    content: "",
    createdAt: "2026-08-13T00:00:00.000Z",
    isStreaming: true,
    isLoading: true,
  };
}

/** The user's question, already on screen before the run is started. */
function userQuestion(): ChatMessage {
  return {
    id: QUESTION_ID,
    role: "user",
    name: "Alice",
    content: "hi",
    createdAt: "2026-08-13T00:00:00.000Z",
  };
}

/**
 * Drives the hook and keeps the history it produces observable to the test.
 *
 * `initial` defaults to a message the reducer has already created. Pass a
 * transcript WITHOUT one to reach the state a refusal arrives in: the run is
 * started, no frame has been folded in yet, so nothing is in flight.
 */
function harness(initial: readonly ChatMessage[] = [pendingAssistant()]): {
  readonly api: { current: UseChatStreamTransportResult | undefined };
  readonly history: { current: readonly ChatMessage[] };
  readonly agentEvents: unknown[];
  readonly errors: string[];
  readonly Probe: () => null;
} {
  const api: { current: UseChatStreamTransportResult | undefined } = {
    current: undefined,
  };
  const history: { current: readonly ChatMessage[] } = { current: initial };
  const agentEvents: unknown[] = [];
  const errors: string[] = [];

  function Probe(): null {
    api.current = useChatStreamTransport({
      setChatHistory: (updater) => {
        history.current = updater(history.current);
      },
      context: { name: "Agent", now: () => "2026-08-13T00:00:00.000Z" },
      onAgentEvent: (frame) => agentEvents.push(frame),
      onStreamError: (reason) => errors.push(reason),
    });
    return null;
  }

  return { api, history, agentEvents, errors, Probe };
}

function nodeEvent(payload: Record<string, unknown>): string {
  // The live Rust/Main replay currently serialises its absent turn link as
  // JSON null rather than omitting it. Keep that exact shape here: treating
  // only `undefined` as absent passed locally while fresh responses still
  // lost their regeneration identity in the browser.
  return JSON.stringify({ message_id: MESSAGE_ID, question_id: null, ...payload });
}

beforeEach(() => {
  registry = installTestEventSource();
  globals["elitea_ui_config"] = {
    vite_server_url: BASE,
    vite_base_uri: "/",
    vite_public_project_id: "public-1",
  };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  registry.restore();
  delete globals["elitea_ui_config"];
  resetConfigForTests();
  resetGeneratedClient();
});

function okStart(
  body: Record<string, unknown> = {
    task_id: "exec-1",
    events_url: EVENTS_URL,
    response_message_id: RESPONSE_MESSAGE_ID,
  },
): void {
  server.use(
    http.post(`${BASE}/elitea_core/messages/prompt_lib/7/uuid-1`, () =>
      HttpResponse.json(body),
    ),
  );
}

/** Start a run and wait for its stream, the preamble every case below shares. */
async function started(api: {
  current: UseChatStreamTransportResult | undefined;
}): Promise<void> {
  await act(async () => {
    await api.current?.start(START);
  });
  await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
}

const START = {
  projectId: 7,
  conversationUuid: "uuid-1",
  contract: "agent.execute.application.v1",
  body: { question: "hi", question_id: QUESTION_ID },
};

describe("useChatStreamTransport", () => {
  it.each([null, "", undefined])("preserves the original question after a resume with an absent frame link: %s", async (questionId) => {
    server.use(http.post(`${BASE}/elitea_core/continue_predict/prompt_lib/7/uuid-1`, () =>
      HttpResponse.json({ task_id: "exec-1", events_url: EVENTS_URL, response_message_id: MESSAGE_ID }),
    ));
    const { api, history, Probe } = harness([
      userQuestion(), { ...pendingAssistant(), questionId: QUESTION_ID },
    ]);
    render(<Probe />);
    await act(async () => {
      await expect(api.current?.resume({
        projectId: 7, conversationUuid: "uuid-1",
        contract: "agent.continue.authorization.v1",
        body: { message_id: MESSAGE_ID, authorization_request_id: "auth-1", authorization_action: "skip" },
      })).resolves.toBe(true);
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    act(() => {
      for (const type of ["agent_start", "chat_predict_summary_started", "agent_llm_chunk", "pipeline_finish"]) {
        registry.emit("execution.node_event", nodeEvent({ type, question_id: questionId, content: "Completed after Skip" }));
      }
    });
    expect(history.current).toHaveLength(2);
    expect(history.current[1]?.questionId).toBe(QUESTION_ID);
    expect(history.current[1]?.isStreaming).toBe(false);
    expect(history.current[0]?.content).toBe("hi");
  });

  it("renders a real recorded turn end to end, from POST through SSE to chat history", async () => {
    okStart();
    const { api, history, Probe } = harness();
    render(<Probe />);

    await act(async () => {
      await expect(api.current?.start(START)).resolves.toBe(true);
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    expect(registry.getOpen()[0]?.url).toContain(EVENTS_URL);

    // The order a live stack emitted, captured from the chat smoke.
    act(() => {
      registry.emit("execution.node_event", nodeEvent({ type: "agent_start" }));
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_on_transitional_edge" }),
      );
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_start" }),
      );
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "MOCK: " }),
      );
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "chat smoke" }),
      );
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_end" }),
      );
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "pipeline_finish" }),
      );
    });

    expect(history.current).toHaveLength(1);
    expect(history.current[0]?.content).toBe("MOCK: chat smoke");
    // The recorded frames carry `question_id: null`. The request still
    // owns that identity, so the live answer must be regeneratable before a
    // reload restores its persisted reply edge.
    expect(history.current[0]?.questionId).toBe(QUESTION_ID);
    expect(history.current[0]?.isStreaming).toBe(false);
    expect(history.current[0]?.isLoading).toBe(false);
  });

  it("reports isStreaming false once the turn ends, without waiting for a close", async () => {
    // The regression this pins shipped and was caught only by the #284
    // journey ("the composer must be released when the turn ends"). The
    // server NEVER closes this stream — executions/events.go keeps it open and
    // emits `: heartbeat` comments — so a transport that waits for a close to
    // stop reporting isStreaming reports it forever. ChatBox gates BOTH the
    // Stop button and the composer on that flag, so the composer stayed
    // disabled for the rest of the session after the first answer.
    //
    // Deliberately asserts the flag AND that nothing is left subscribed: a
    // transport that flipped the flag but kept the socket would still leak
    // frames into the next conversation, which is #328.
    okStart();
    const { api, history, Probe } = harness();
    render(<Probe />);

    await act(async () => {
      await api.current?.start(START);
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "done" }),
      );
    });
    expect(api.current?.isStreaming).toBe(true);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "pipeline_finish" }),
      );
    });

    await waitFor(() => expect(api.current?.isStreaming).toBe(false));
    expect(registry.getOpen()).toHaveLength(0);
    expect(history.current[0]?.isStreaming).toBe(false);
  });

  /**
   * DEFECT: `isTurnTerminalFrame` accepted only `pipeline_finish` and an
   * `agent_response` carrying a `finish_reason`. A run that PAUSES emits
   * neither: the worker's `emit_terminal` returns `agent_hitl_interrupt` or
   * `mcp_authorization_required` and stops. The server never closes the stream
   * either, so the connection stayed open forever. `isStreaming` stayed true.
   * ChatBox kept the composer disabled for the rest of the session. The
   * stream held one of the principal's four SSE admission slots.
   *
   * EVIDENCE: `handlers/agent_events.py:293-360` (emit_terminal) and
   * `executions/events.go:181-232` (the server loop has no terminal exit).
   */
  it("releases the stream when the run pauses for a human decision", async () => {
    okStart();
    const { api, history, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({
          type: "agent_hitl_interrupt",
          content: "Approve this tool call?",
          response_metadata: {
            thread_id: "t-1",
            message: "Approve this tool call?",
            hitl_interrupt: { tool_name: "shell" },
          },
        }),
      );
    });

    await waitFor(() => expect(api.current?.isStreaming).toBe(false));
    expect(registry.getOpen()).toHaveLength(0);
    expect(history.current[0]?.isStreaming).toBe(false);
  });

  /**
   * The counterpart guard: a fan-out CHILD pause is not the end of the run.
   * Its siblings keep producing frames on the SAME stream, so detaching there
   * would truncate their output. `classifyHitlPause` is the one rule both the
   * reducer and the transport read.
   */
  it("keeps the stream open when only a fan-out child pauses", async () => {
    okStart();
    const { api, history, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({
          type: "agent_hitl_interrupt",
          content: "child paused",
          response_metadata: {
            message: "child paused",
            metadata: { parent_agent_name: "root", child_thread_id: "child-1" },
          },
        }),
      );
    });

    expect(api.current?.isStreaming).toBe(true);
    expect(registry.getOpen()).toHaveLength(1);

    // A sibling still folds into the same message, which is what the open
    // stream is for.
    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "sibling output" }),
      );
    });
    expect(history.current[0]?.content).toContain("sibling output");
  });

  /**
   * An in-process PARALLEL AGGREGATE pause IS the end of the run, unlike a
   * fan-out child. One invoke spawns the sub-agents in one process and returns
   * every pause in one frame. The worker emits that frame as the
   * execution terminal (`emit_terminal`, agent_events.py:293-330). Nothing
   * follows it, so a transport that stays attached locks the composer for the
   * rest of the session.
   *
   * The message keeps `isStreaming: true`. That is the reducer's own rule for
   * both parallel shapes, and it holds the live thinking view open. Only the
   * TRANSPORT detaches here.
   */
  it("closes on an in-process parallel aggregate pause", async () => {
    okStart();
    const { api, history, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({
          type: "agent_hitl_interrupt",
          content: "Approve both sub-agents?",
          response_metadata: {
            thread_id: "t-1",
            message: "Approve both sub-agents?",
            hitl_interrupts: [
              { tool_call_id: "c-1", parent_agent_name: "researcher" },
              { tool_call_id: "c-2", parent_agent_name: "writer" },
            ],
          },
        }),
      );
    });

    await waitFor(() => expect(api.current?.isStreaming).toBe(false));
    expect(registry.getOpen()).toHaveLength(0);
    // The reducer still marks the message live, which keeps the thinking view
    // open for the siblings that have not rendered a card.
    expect(history.current[0]?.isStreaming).toBe(true);
  });

  /**
   * `mcp_authorization_required` is emitted TWICE for one run: once as progress
   * from the tool-error path, and once as the execution terminal. Only the
   * terminal frame carries the `authorization_requests` array
   * (`agent_events.py:335-357`). Detaching on the first would drop the second
   * authorization card.
   */
  it("closes on the terminal MCP authorization frame only, not the progress one", async () => {
    okStart();
    const { api, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({
          type: "mcp_authorization_required",
          content: "Authorization required.",
          response_metadata: {
            tool_run_id: "run-1",
            tool_name: "github",
            server_url: "https://mcp.example",
            authorization_servers: ["https://auth.example"],
          },
        }),
      );
    });
    expect(api.current?.isStreaming).toBe(true);
    expect(registry.getOpen()).toHaveLength(1);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({
          type: "mcp_authorization_required",
          content: "Toolkit authorization is required.",
          response_metadata: {
            tool_run_id: "run-2",
            tool_name: "github",
            server_url: "https://mcp.example",
            authorization_servers: ["https://auth.example"],
            authorization_requests: [
              { tool_run_id: "run-1" },
              { tool_run_id: "run-2" },
            ],
          },
        }),
      );
    });

    await waitFor(() => expect(api.current?.isStreaming).toBe(false));
    expect(registry.getOpen()).toHaveLength(0);
  });

  /** A failure frame also ends the turn — the worker emits no node event after one. */
  it("releases the stream when the run fails", async () => {
    okStart();
    const { api, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({
          type: "llm_error",
          content: "the model refused the request",
        }),
      );
    });

    await waitFor(() => expect(api.current?.isStreaming).toBe(false));
    expect(registry.getOpen()).toHaveLength(0);
  });

  it("reports false and opens NO stream when the backend rejects the contract", async () => {
    // The documented fallback signal: the caller then emits chat_predict.
    server.use(
      http.post(
        `${BASE}/elitea_core/messages/prompt_lib/7/uuid-1`,
        () => new HttpResponse(null, { status: 400 }),
      ),
    );
    const { api, Probe } = harness();
    render(<Probe />);

    await act(async () => {
      await expect(api.current?.start(START)).resolves.toBe(false);
    });
    expect(registry.getOpen()).toHaveLength(0);
  });

  it("preserves a typed 422 refusal and its server message for the chat widget", async () => {
    server.use(
      http.post(`${BASE}/elitea_core/messages/prompt_lib/7/uuid-1`, () =>
        HttpResponse.json(
          {
            error: "unsupported_agent_execution",
            message: "This agent turn requires the current execution path.",
          },
          { status: 422 },
        ),
      ),
    );
    const { api, Probe } = harness();
    render(<Probe />);

    await act(async () => {
      await expect(api.current?.startDetailed(START)).resolves.toEqual({
        started: false,
        reason: "rejected",
        message: "This agent turn requires the current execution path.",
      });
    });
    expect(registry.getOpen()).toHaveLength(0);
  });

  it("reports false when a 200 carries no events_url", async () => {
    // An older backend answering the same route. Treating it as success would
    // leave the run unwatched AND suppress the socket fallback.
    okStart({ task_id: "exec-1" });
    const { api, Probe } = harness();
    render(<Probe />);

    await act(async () => {
      await expect(api.current?.start(START)).resolves.toBe(false);
    });
    expect(registry.getOpen()).toHaveLength(0);
  });

  it("forwards graph frames to the flow editor but never the chunks", async () => {
    okStart();
    const { api, agentEvents, Probe } = harness();
    render(<Probe />);
    await act(async () => {
      await api.current?.start(START);
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_on_tool_node" }),
      );
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "x" }),
      );
    });

    expect(
      agentEvents.map((frame) => (frame as { type: string }).type),
    ).toEqual(["agent_on_tool_node"]);
  });

  it("stops the spinner when the run fails server-side, and says why", async () => {
    okStart();
    const { api, history, errors, Probe } = harness();
    render(<Probe />);
    await act(async () => {
      await api.current?.start(START);
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));

    act(() => {
      registry.emit(
        "execution.failed",
        JSON.stringify({ code: "INTERNAL", safe_message: "model unavailable" }),
      );
    });

    expect(history.current[0]?.isStreaming).toBe(false);
    expect(history.current[0]?.isLoading).toBe(false);
    expect(history.current[0]?.exception).toBe("model unavailable");
    expect(errors).toEqual(["model unavailable"]);
  });

  it("shows the server's own sentence, which it sends as safe_message", async () => {
    // Measured on a live stack: the native Rust runtime refused an agent
    // profile and Main durably recorded exactly this payload in
    // `elitea_runtime.execution_replay_events`. The transport read `error`,
    // a key no producer of `execution.failed` writes — every one of them
    // emits `{code, safe_message, retryable}` — so the user got the generic
    // "The agent run failed." instead of the reason.
    okStart();
    const { api, history, errors, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.failed",
        JSON.stringify({
          code: "UNSUPPORTED_CAPABILITY",
          safe_message: "Configuration type is not supported.",
          retryable: false,
        }),
      );
    });

    expect(history.current[0]?.exception).toBe(
      "Configuration type is not supported.",
    );
    expect(errors).toEqual(["Configuration type is not supported."]);
  });

  it("puts a refusal on screen when it beat the first frame, so no message exists yet", async () => {
    // The second half of the same live observation: the refusal was the run's
    // FIRST event, so the reducer had never created an assistant message —
    // `settleInFlight` matched nothing, returned the history unchanged, and
    // the composer re-enabled over a transcript that said nothing at all.
    okStart();
    const { api, history, Probe } = harness([userQuestion()]);
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.failed",
        JSON.stringify({
          code: "UNSUPPORTED_CAPABILITY",
          safe_message: "Configuration type is not supported.",
          retryable: false,
        }),
      );
    });

    expect(history.current).toHaveLength(2);
    const failure = history.current[1];
    expect(failure?.role).toBe("assistant");
    // `exception` is what `ApplicationAnswer` renders through `ErrorTrace`.
    expect(failure?.exception).toBe("Configuration type is not supported.");
    expect(failure?.isStreaming).toBe(false);
    expect(failure?.isLoading).toBe(false);
    // Captured before `detach` clears it, so regenerate can still find the
    // question this refused turn answered.
    expect(failure?.questionId).toBe(QUESTION_ID);
    expect(failure?.id).toBe(RESPONSE_MESSAGE_ID);
  });

  it("keeps the user's question when the turn is refused", async () => {
    // Observed once in the browser as a question that vanished on refusal.
    // Nothing in this transport removes it — the assertion pins that.
    okStart();
    const { api, history, Probe } = harness([userQuestion()]);
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.failed",
        JSON.stringify({ code: "INTERNAL", safe_message: "nope" }),
      );
    });

    expect(history.current[0]).toEqual(userQuestion());
  });

  it("names the failure by its code when the payload carries no sentence", async () => {
    okStart();
    const { api, history, errors, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.failed",
        JSON.stringify({ code: "DEADLINE_EXCEEDED", retryable: true }),
      );
    });

    expect(history.current[0]?.exception).toBe("DEADLINE_EXCEEDED");
    expect(errors).toEqual(["DEADLINE_EXCEEDED"]);
  });

  it("does not settle the message on the first drop — that turn is still resumable", async () => {
    // Before #329 a drop ended the turn on the spot. It is now a reconnect,
    // and settling here would render "done" over an answer still coming.
    okStart();
    const { api, history, errors, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.fail();
    });

    expect(history.current[0]?.isStreaming).toBe(true);
    expect(history.current[0]?.isLoading).toBe(true);
    expect(history.current[0]?.exception).toBeUndefined();
    expect(errors).toEqual([]);
  });

  it("does not reopen the stream when the context changes mid-answer", async () => {
    // A reconnect would replay the run from its cursor and duplicate what is
    // already on screen, which is why the context is read through a ref.
    okStart();
    let renderCount = 0;
    const api: { current: UseChatStreamTransportResult | undefined } = {
      current: undefined,
    };
    const history: { current: readonly ChatMessage[] } = {
      current: [pendingAssistant()],
    };

    function Probe(): null {
      renderCount += 1;
      api.current = useChatStreamTransport({
        setChatHistory: (updater) => {
          history.current = updater(history.current);
        },
        // A new object identity on every render — the realistic case.
        context: { name: `Agent ${String(renderCount)}` },
      });
      return null;
    }

    const { rerender } = render(<Probe />);
    await act(async () => {
      await api.current?.start(START);
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));

    rerender(<Probe />);
    rerender(<Probe />);

    expect(registry.getSources()).toHaveLength(1);
    expect(registry.getOpen()).toHaveLength(1);
  });

  it("closes the stream on request", async () => {
    okStart();
    const { api, Probe } = harness();
    render(<Probe />);
    await act(async () => {
      await api.current?.start(START);
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));

    act(() => {
      api.current?.close();
    });

    await waitFor(() => expect(registry.getOpen()).toHaveLength(0));
  });

  it("ignores a frame that names no type instead of forwarding it", async () => {
    okStart();
    const { api, history, agentEvents, Probe } = harness();
    render(<Probe />);
    await act(async () => {
      await api.current?.start(START);
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));

    const before = history.current;
    act(() => {
      registry.emit(
        "execution.node_event",
        JSON.stringify({ message_id: MESSAGE_ID }),
      );
    });

    expect(history.current).toBe(before);
    expect(agentEvents).toHaveLength(0);
  });
});

/* ------------------------------------------------------------------ */
/*  #328 — a stream belongs to the conversation that started it          */
/* ------------------------------------------------------------------ */

/** Two conversations, two histories, one never-unmounted hook — the real ChatBox shape. */
function conversationHarness(): {
  readonly api: { current: UseChatStreamTransportResult | undefined };
  readonly first: { current: readonly ChatMessage[] };
  readonly second: { current: readonly ChatMessage[] };
  readonly Probe: (props: {
    conversationUuid: string;
    target: { current: readonly ChatMessage[] };
  }) => null;
} {
  const api: { current: UseChatStreamTransportResult | undefined } = {
    current: undefined,
  };
  const first: { current: readonly ChatMessage[] } = {
    current: [pendingAssistant()],
  };
  const second: { current: readonly ChatMessage[] } = { current: [] };

  function Probe({
    conversationUuid,
    target,
  }: {
    conversationUuid: string;
    target: { current: readonly ChatMessage[] };
  }): null {
    api.current = useChatStreamTransport({
      conversationUuid,
      setChatHistory: (updater) => {
        target.current = updater(target.current);
      },
      context: { name: "Agent", now: () => "2026-08-13T00:00:00.000Z" },
    });
    return null;
  }

  return { api, first, second, Probe };
}

describe("stream ownership (#328)", () => {
  it("does not leak the first conversation's frames into the second one", async () => {
    okStart();
    const { api, first, second, Probe } = conversationHarness();
    const { rerender } = render(
      <Probe conversationUuid="uuid-1" target={first} />,
    );
    await started(api);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "first answer" }),
      );
    });
    expect(first.current[0]?.content).toBe("first answer");

    // The user opens another conversation. ChatBox does NOT unmount — it
    // re-renders against a different conversation and a different history.
    rerender(<Probe conversationUuid="uuid-2" target={second} />);
    // Asserted BEFORE any further frame: a terminal frame would close the
    // stream on its own, which would let this pass with no switch handling
    // at all.
    expect(registry.getOpen()).toHaveLength(0);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "LEAKED" }),
      );
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_response", content: "LEAKED WHOLE" }),
      );
      registry.emit(
        "execution.failed",
        JSON.stringify({ code: "INTERNAL", safe_message: "LEAKED FAILURE" }),
      );
    });

    // The assertion that matters: nothing from the abandoned run reached the
    // transcript now on screen. Asserting `close()` fired would pass even if
    // the frames still landed.
    expect(second.current).toEqual([]);
    expect(first.current[0]?.content).toBe("first answer");
  });

  it("subscribes to nothing when the user switches while the start POST is in flight", async () => {
    // The run exists server-side by then, so `start` still answers `true` (a
    // `chat_predict` fallback would run the agent twice) — but its frames
    // belong to a transcript that is no longer on screen.
    okStart();
    const { api, second, Probe } = conversationHarness();
    const { rerender } = render(
      <Probe conversationUuid="uuid-1" target={second} />,
    );

    const pending = api.current?.start(START);
    // A synchronous `act` so the switch is COMMITTED before the POST resolves
    // — the ordering the defect needs.
    act(() => {
      rerender(<Probe conversationUuid="uuid-2" target={second} />);
    });
    let owned: boolean | undefined;
    await act(async () => {
      owned = await pending;
    });

    expect(owned).toBe(true);
    expect(registry.getSources()).toHaveLength(0);
    expect(second.current).toEqual([]);
  });

  it("opens no stream after unmount, including a reconnect already scheduled", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      okStart();
      const { api, history, Probe } = harness();
      const { unmount } = render(<Probe />);
      await started(api);

      act(() => {
        registry.fail();
      });
      const before = history.current;
      unmount();

      act(() => {
        vi.advanceTimersByTime(60_000);
      });

      // One source ever: the original. A pending reconnect that survived
      // unmount would open a second, feeding a hook nothing renders.
      expect(registry.getSources()).toHaveLength(1);
      expect(registry.getOpen()).toHaveLength(0);
      expect(
        registry.emit(
          "execution.node_event",
          nodeEvent({ type: "agent_llm_chunk", content: "after unmount" }),
        ),
      ).toBe(0);
      expect(history.current).toBe(before);
    } finally {
      vi.useRealTimers();
    }
  });
});

/* ------------------------------------------------------------------ */
/*  #328 — Stop                                                          */
/* ------------------------------------------------------------------ */

describe("stop (#328)", () => {
  it("cancels the run server-side, closes the stream, and applies nothing further", async () => {
    okStart();
    const cancelled: string[] = [];
    server.use(
      http.delete(
        `${BASE}/elitea_core/task/prompt_lib/7/:responseMessageId`,
        ({ params }) => {
          cancelled.push(String(params["responseMessageId"]));
          return new HttpResponse(null, { status: 204 });
        },
      ),
    );
    const { api, history, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "half an ans" }),
      );
      api.current?.stop();
    });

    await waitFor(() => expect(registry.getOpen()).toHaveLength(0));
    // Closing the client stream would leave the agent running and billing;
    // the DELETE addresses the response message the start endpoint named.
    await waitFor(() => expect(cancelled).toEqual([RESPONSE_MESSAGE_ID]));
    expect(history.current[0]?.isStreaming).toBe(false);
    expect(history.current[0]?.isLoading).toBe(false);

    const settled = history.current;
    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: " MORE" }),
      );
    });
    expect(history.current).toBe(settled);
    expect(history.current[0]?.content).toBe("half an ans");
  });

  it("does not reconnect after a stop", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      okStart();
      server.use(
        http.delete(
          `${BASE}/elitea_core/task/prompt_lib/7/:id`,
          () => new HttpResponse(null, { status: 204 }),
        ),
      );
      const { api, Probe } = harness();
      render(<Probe />);
      await started(api);

      act(() => {
        api.current?.stop();
        registry.fail();
        vi.advanceTimersByTime(60_000);
      });

      expect(registry.getSources()).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });
});

/* ------------------------------------------------------------------ */
/*  #329 — reconnect, resume, backoff                                    */
/* ------------------------------------------------------------------ */

describe("resume after a drop (#329)", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("reopens from the last cursor and finishes the answer without duplicating it", async () => {
    okStart();
    const { api, history, errors, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "MOCK: " }),
        "11",
      );
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "chat " }),
        "12",
      );
    });
    expect(history.current[0]?.content).toBe("MOCK: chat ");

    act(() => {
      registry.fail();
      vi.advanceTimersByTime(999);
    });
    expect(registry.getOpen()).toHaveLength(0);

    act(() => {
      vi.advanceTimersByTime(1);
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    // The whole point: `cursor` is what `events.go`'s `requestedCursor` reads
    // as `Last-Event-ID`, so the server replays only frames after id 12.
    expect(registry.getOpen()[0]?.url).toContain(
      "/executions/7/exec-1/events?cursor=12",
    );

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "smoke" }),
        "13",
      );
      // The tail `agent_response` carries the WHOLE answer, as the live stack
      // emits it. Nothing may be rendered twice.
      registry.emit(
        "execution.node_event",
        nodeEvent({
          type: "agent_response",
          content: "MOCK: chat smoke",
          response_metadata: { finish_reason: "stop" },
        }),
        "14",
      );
    });

    expect(history.current[0]?.content).toBe("MOCK: chat smoke");
    expect(history.current[0]?.isStreaming).toBe(false);
    expect(errors).toEqual([]);
  });

  it("still reports isStreaming across the gap, so Stop does not turn back into Send", async () => {
    okStart();
    const { api, Probe } = harness();
    render(<Probe />);
    await started(api);
    expect(api.current?.isStreaming).toBe(true);

    act(() => {
      registry.fail();
      vi.advanceTimersByTime(500);
    });

    // Nothing is subscribed right now — and the turn is still running.
    expect(registry.getOpen()).toHaveLength(0);
    expect(api.current?.isStreaming).toBe(true);
  });

  it("resumes from cursor 0 — i.e. no cursor at all — when the drop preceded every frame", async () => {
    okStart();
    const { api, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.fail();
      vi.advanceTimersByTime(1_000);
    });

    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    // Asking for `?cursor=` or `?cursor=undefined` would be a 400 from
    // `strconv.ParseUint`; the whole run replays instead, and `agent_start`
    // resets the bubble so the replay cannot double anything.
    expect(registry.getOpen()[0]?.url).not.toContain("cursor");
  });

  it("bounds the retries at four, then settles the message and says the connection was lost", async () => {
    okStart();
    const { api, history, errors, Probe } = harness();
    render(<Probe />);
    await started(api);

    // 1s, 2s, 4s, 8s — `streamReconnectDelayMs`. Each step is asserted for
    // BOTH halves: nothing reopens a millisecond early, and it does reopen.
    for (const delay of [1_000, 2_000, 4_000, 8_000]) {
      const opened = registry.getSources().length;
      act(() => {
        registry.fail();
        vi.advanceTimersByTime(delay - 1);
      });
      expect(registry.getSources()).toHaveLength(opened);
      act(() => {
        vi.advanceTimersByTime(1);
      });
      // eslint-disable-next-line no-await-in-loop -- sequential by construction: each backoff step must be observed before the next drop.
      await waitFor(() =>
        expect(registry.getSources()).toHaveLength(opened + 1),
      );
    }

    // The fifth failure is where the budget runs out.
    act(() => {
      registry.fail();
      vi.advanceTimersByTime(600_000);
    });

    expect(registry.getSources()).toHaveLength(5);
    expect(registry.getOpen()).toHaveLength(0);
    expect(history.current[0]?.isStreaming).toBe(false);
    expect(history.current[0]?.isLoading).toBe(false);
    // The reason goes ON the message, not only to `onStreamError`: the
    // callback drives a toast that is gone in seconds, while the transcript is
    // what the user still has when they come back to the tab.
    expect(history.current[0]?.exception).toBe(
      "The connection to the agent run was lost.",
    );
    expect(errors).toEqual(["The connection to the agent run was lost."]);
  });

  it("still says the connection was lost when the stream never delivered a frame", async () => {
    // The same hole as the early refusal, on the other path: a stream that
    // dies before its first frame leaves nothing in flight for
    // `settleInFlight` to mark, so the turn ended with an untouched
    // transcript and a silently re-enabled composer.
    okStart();
    const { api, history, Probe } = harness([userQuestion()]);
    render(<Probe />);
    await started(api);

    for (const delay of [1_000, 2_000, 4_000, 8_000]) {
      act(() => {
        registry.fail();
        vi.advanceTimersByTime(delay);
      });
    }
    act(() => {
      registry.fail();
      vi.advanceTimersByTime(600_000);
    });

    expect(history.current).toHaveLength(2);
    expect(history.current[0]).toEqual(userQuestion());
    expect(history.current[1]?.role).toBe("assistant");
    expect(history.current[1]?.exception).toBe(
      "The connection to the agent run was lost.",
    );
  });

  it("spends a fresh budget after a delivered frame, not the one the last outage exhausted", async () => {
    okStart();
    const { api, history, errors, Probe } = harness();
    render(<Probe />);
    await started(api);

    for (const delay of [1_000, 2_000, 4_000, 8_000]) {
      act(() => {
        registry.fail();
        vi.advanceTimersByTime(delay);
      });
      // eslint-disable-next-line no-await-in-loop -- sequential by construction.
      await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    }

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "recovered" }),
        "7",
      );
    });
    act(() => {
      registry.fail();
      vi.advanceTimersByTime(1_000);
    });

    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    expect(registry.getOpen()[0]?.url).toContain("cursor=7");
    expect(history.current[0]?.isStreaming).toBe(true);
    expect(errors).toEqual([]);
  });

  it("does not reconnect once the turn has finished", async () => {
    // The server closes a finished stream, which reaches the client as an
    // `error` event exactly like a drop. Retrying it would reopen a stream
    // with nothing left to send, four times, per completed answer.
    okStart();
    const { api, errors, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "pipeline_finish" }),
        "9",
      );
      registry.fail();
      vi.advanceTimersByTime(600_000);
    });

    expect(registry.getSources()).toHaveLength(1);
    expect(errors).toEqual([]);
  });

  it("keeps streaming through a replay_reset instead of treating the pruned log as a failure", async () => {
    okStart();
    const { api, history, errors, Probe } = harness();
    render(<Probe />);
    await started(api);

    act(() => {
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "before " }),
        "3",
      );
      registry.fail();
      vi.advanceTimersByTime(1_000);
    });
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));

    act(() => {
      // The resume landed past the retention window: frames between cursor 3
      // and 40 are gone for good.
      registry.emit(
        "execution.replay_reset",
        JSON.stringify({ reason: "progress_retention_window_elapsed" }),
        "40",
      );
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "agent_llm_chunk", content: "after" }),
        "41",
      );
      registry.emit(
        "execution.node_event",
        nodeEvent({ type: "pipeline_finish" }),
        "42",
      );
    });

    // A hole in the middle, and a turn that still completes — not an error,
    // and not a reconnect loop back onto the pruned cursor.
    expect(history.current[0]?.content).toBe("before after");
    expect(history.current[0]?.isStreaming).toBe(false);
    expect(errors).toEqual([]);
    expect(registry.getSources()).toHaveLength(2);
  });
});

describe("the send path starts a run exactly once", () => {
  it("never runs both transports for one question", async () => {
    // The whole point of `start` returning a boolean: the execution exists
    // server-side the moment the POST succeeds, so an additional chat_predict
    // would run the agent — and bill it — a second time.
    const posts = vi.fn();
    server.use(
      http.post(`${BASE}/elitea_core/messages/prompt_lib/7/uuid-1`, () => {
        posts();
        return HttpResponse.json({ task_id: "exec-1", events_url: EVENTS_URL });
      }),
    );
    const { api, Probe } = harness();
    render(<Probe />);

    let owned: boolean | undefined;
    await act(async () => {
      owned = await api.current?.start(START);
    });

    expect(owned).toBe(true);
    expect(posts).toHaveBeenCalledTimes(1);
  });
});

describe("the regeneration path owns its replacement stream", () => {
  it("uses the current contract and subscribes to the accepted execution", async () => {
    let requestUrl = "";
    let requestBody: unknown;
    server.use(
      http.post(
        `${BASE}/elitea_core/regenerate/prompt_lib/7/${RESPONSE_MESSAGE_ID}`,
        async ({ request }) => {
          requestUrl = request.url;
          requestBody = await request.json();
          return HttpResponse.json({
            task_id: "exec-1",
            events_url: EVENTS_URL,
            response_message_id: RESPONSE_MESSAGE_ID,
          });
        },
      ),
    );
    const { api, Probe } = harness();
    render(<Probe />);

    await act(async () => {
      await expect(
        api.current?.regenerate({
          projectId: 7,
          conversationUuid: "uuid-1",
          responseMessageId: RESPONSE_MESSAGE_ID,
          body: { message_id: RESPONSE_MESSAGE_ID },
        }),
      ).resolves.toBe(true);
    });

    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    expect(new URL(requestUrl).searchParams.get("execution_contract")).toBe(
      "agent.regenerate.v1",
    );
    expect(requestBody).toEqual({ message_id: RESPONSE_MESSAGE_ID });
    expect(registry.getOpen()[0]?.url).toContain(EVENTS_URL);
  });

  /**
   * The still-finalizing refusal, read off the REAL response rather than a
   * hand-built error.
   *
   * This is the only place in the app that can tell `retry-later` from every
   * other failure, and the distinction is not cosmetic: `no-transport` sends
   * the caller to the legacy REST trigger, which posts the same regeneration
   * WITHOUT an `execution_contract` and is answered 400 — so a widget-level
   * retry can only ever work if this classification happens here first.
   */
  it("reports the still-finalizing 409 as retry-later, not as an absent transport", async () => {
    server.use(
      http.post(
        `${BASE}/elitea_core/regenerate/prompt_lib/7/${RESPONSE_MESSAGE_ID}`,
        () =>
          HttpResponse.json(
            {
              error: "agent_regeneration_pending",
              message: "The previous agent response is still being finalized.",
              retryable: true,
            },
            { status: 409, headers: { "Retry-After": "1" } },
          ),
      ),
    );
    const { api, Probe } = harness();
    render(<Probe />);

    await act(async () => {
      await expect(
        api.current?.regenerateDetailed({
          projectId: 7,
          conversationUuid: "uuid-1",
          responseMessageId: RESPONSE_MESSAGE_ID,
          body: { message_id: RESPONSE_MESSAGE_ID },
        }),
      ).resolves.toEqual({ started: false, reason: "retry-later" });
    });

    // Refused ⇒ no run to watch. A subscription here would leave a stream open
    // for an execution the server never created.
    expect(registry.getOpen()).toHaveLength(0);
  });

  /**
   * The OTHER 409s on the same route state `retryable: false`
   * (`agent_hitl_already_resolved`, `agent_authorization_already_resolved`) —
   * repeating those never succeeds, so they must keep the fallback behaviour.
   */
  it("does NOT read a non-retryable 409 as retry-later", async () => {
    server.use(
      http.post(
        `${BASE}/elitea_core/regenerate/prompt_lib/7/${RESPONSE_MESSAGE_ID}`,
        () =>
          HttpResponse.json(
            { error: "agent_hitl_already_resolved", retryable: false },
            { status: 409 },
          ),
      ),
    );
    const { api, Probe } = harness();
    render(<Probe />);

    await act(async () => {
      await expect(
        api.current?.regenerateDetailed({
          projectId: 7,
          conversationUuid: "uuid-1",
          responseMessageId: RESPONSE_MESSAGE_ID,
          body: { message_id: RESPONSE_MESSAGE_ID },
        }),
      ).resolves.toEqual({ started: false, reason: "no-transport" });
    });
  });
});
