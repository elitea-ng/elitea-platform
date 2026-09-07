/**
 * Regression cover for the two silent-drop defects in `useChatBoxHandlers`.
 *
 * DEFECT 1 — a send that reached no transport reported success.
 * `sendQuestion` fell back to `emitSocket('chat_predict', …)` whenever the
 * REST start did not take the run, and then IGNORED the emit's return value.
 * On a deployment whose `/app/config.js` serves `vite_socket_server: ""` the
 * injected client is `createNoopSocketClient()`, whose `emit` is `() => false`
 * (`shared/api/socket/client.ts`) — so the question was dropped with no
 * answer, no error and a `{ success: true }` result that told the composer to
 * clear the user's text.
 *
 * DEFECT 2 — a continuation that reached no transport left the bubble stuck.
 * `continueHitl` / `resumeMcpFlow` / `continueTokenLimit` first wipe the
 * approval card and set `isLoading`/`isStreaming` on the message, then emit
 * `chat_continue_predict` into the same no-op client. The wipe was
 * unconditional and irreversible, so the run stayed paused server-side while
 * the card was gone and the message spun for the rest of the session.
 *
 * `useChatBoxHandlers` calls no React hook of its own — it only builds
 * closures over `deps` — so these tests invoke it directly.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ChatMessage } from "@/features/chat-messages";
import { EliteaApiError } from "@/shared/api/generated/mutator";
import { ROLES } from "@/shared/lib/enums";

import { useChatBoxHandlers } from "./useChatBoxHandlers";
import { regenerationStillFinalizingText } from "./useChatBoxHandlers.regenerate";
import type {
  ChatBoxHandlerDeps,
  StreamStartOutcome,
} from "./useChatBoxHandlers.helpers";

/** A `setChatHistory` backed by a plain array, so a test can read the result. */
function makeHistory(seed: readonly ChatMessage[]): {
  readonly read: () => readonly ChatMessage[];
  readonly setChatHistory: ChatBoxHandlerDeps["setChatHistory"];
} {
  let current = seed;
  return {
    read: () => current,
    setChatHistory: (update) => {
      current = typeof update === "function" ? update(current) : update;
    },
  };
}

function makeDeps(
  overrides: Partial<ChatBoxHandlerDeps> & {
    readonly setChatHistory: ChatBoxHandlerDeps["setChatHistory"];
  },
): ChatBoxHandlerDeps {
  return {
    emitSocket: () => true,
    chatHistory: [],
    setStreamingInfo: () => undefined,
    projectId: 1,
    conversationUuid: "conv-uuid-1",
    ...overrides,
  };
}

/** The no-op socket client the app injects when `vite_socket_server` is empty. */
const deadSocket = (): ChatBoxHandlerDeps["emitSocket"] => () => false;

const noTransport: StreamStartOutcome = {
  started: false,
  reason: "no-transport",
};

describe("sendQuestion — a turn no transport accepted", () => {
  it("reports failure and shows the reason when the socket is the no-op stub", async () => {
    const history = makeHistory([]);
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        emitSocket: deadSocket(),
        startStreamedExecution: () => Promise.resolve(noTransport),
      }),
    );

    const result = await handlers.sendQuestion({ question: "hi" });

    expect(result.success).toBe(false);
    const errors = history
      .read()
      .filter((message) => message.exception !== undefined);
    expect(errors).toHaveLength(1);
    expect(errors[0]?.role).toBe(ROLES.Assistant);
    expect(String(errors[0]?.exception)).toContain("was not sent");
    // The question the person typed SURVIVES the refusal. The composer has
    // already been cleared, so this bubble is the only copy of it, and the
    // error message is anchored to it by `questionId`. Journeys 8, 9 and 12
    // read exactly this out of `chat-message-list`.
    const questions = history
      .read()
      .filter((message) => message.role === ROLES.User);
    expect(questions).toHaveLength(1);
    expect(questions[0]?.content).toBe("hi");
    expect(errors[0]?.questionId).toBe(questions[0]?.id);
  });

  it("keeps every failed turn's question, so a second refusal does not erase the first", async () => {
    const history = makeHistory([]);
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        emitSocket: deadSocket(),
        startStreamedExecution: () => Promise.resolve(noTransport),
      }),
    );

    await handlers.sendQuestion({ question: "hi" });
    const [firstQuestion] = history
      .read()
      .filter((message) => message.role === ROLES.User);
    await handlers.sendQuestion({ question: "hi again" });

    const errors = history
      .read()
      .filter((message) => message.exception !== undefined);
    expect(errors).toHaveLength(2);
    expect(new Set(errors.map((message) => message.questionId)).size).toBe(2);
    expect(
      history.read().some((message) => message.id === firstQuestion?.id),
    ).toBe(true);
  });

  it("keeps the turn silent-free but successful when the socket really delivers", async () => {
    const history = makeHistory([]);
    const emitSocket = vi.fn(
      () => true,
    ) as unknown as ChatBoxHandlerDeps["emitSocket"];
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        emitSocket,
        startStreamedExecution: () => Promise.resolve(noTransport),
      }),
    );

    const result = await handlers.sendQuestion({ question: "hi" });

    expect(result.success).toBe(true);
    expect(
      history.read().some((message) => message.exception !== undefined),
    ).toBe(false);
  });

  it("does not emit over the socket at all once the REST start owns the run", async () => {
    const history = makeHistory([]);
    const emitSocket = vi.fn(
      () => true,
    ) as unknown as ChatBoxHandlerDeps["emitSocket"];
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        emitSocket,
        startStreamedExecution: () =>
          Promise.resolve<StreamStartOutcome>({ started: true }),
      }),
    );

    const result = await handlers.sendQuestion({ question: "hi" });

    expect(result.success).toBe(true);
    expect(emitSocket).not.toHaveBeenCalled();
  });

  it("shows the route’s own reason and skips the socket when the start was rejected", async () => {
    const history = makeHistory([]);
    const emitSocket = vi.fn(
      () => true,
    ) as unknown as ChatBoxHandlerDeps["emitSocket"];
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        emitSocket,
        startStreamedExecution: () =>
          Promise.resolve<StreamStartOutcome>({
            started: false,
            reason: "rejected",
            message: "No model is configured.",
          }),
      }),
    );

    const result = await handlers.sendQuestion({ question: "hi" });

    expect(result.success).toBe(false);
    expect(emitSocket).not.toHaveBeenCalled();
    expect(history.read().map((message) => message.exception)).toContain(
      "No model is configured.",
    );
    // A refusal the route explained still keeps the question on screen: it is
    // the only copy of the text, and the reason means nothing without it.
    expect(
      history.read().filter((message) => message.role === ROLES.User),
    ).toHaveLength(1);
  });

  it("does not duplicate a persisted question when a retry is rejected", async () => {
    const persistedQuestion: ChatMessage = {
      id: "persisted-question",
      role: ROLES.User,
      name: "User",
      content: "Write a detailed guide",
      createdAt: "2026-08-28T00:00:00.000Z",
    };
    const history = makeHistory([persistedQuestion]);
    const handlers = useChatBoxHandlers(
      makeDeps({
        chatHistory: [persistedQuestion],
        setChatHistory: history.setChatHistory,
        startStreamedExecution: () =>
          Promise.resolve<StreamStartOutcome>({
            started: false,
            reason: "rejected",
            message: "A previous agent turn is still being recovered.",
          }),
      }),
    );

    await handlers.sendQuestion({ question: "Write a detailed guide" });

    expect(
      history.read().filter((message) => message.role === ROLES.User),
    ).toEqual([persistedQuestion]);
    expect(history.read().at(-1)?.exception).toBe(
      "A previous agent turn is still being recovered.",
    );
  });

  it("reports failure rather than success when no conversation could be created", async () => {
    const history = makeHistory([]);
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        conversationUuid: undefined,
        createConversation: () => Promise.resolve(undefined),
        startStreamedExecution: () => Promise.resolve(noTransport),
      }),
    );

    const result = await handlers.sendQuestion({ question: "hi" });

    expect(result.success).toBe(false);
    expect(
      history.read().some((message) => message.exception !== undefined),
    ).toBe(true);
  });
});

describe("regenerateAnswer — the current REST and SSE contract", () => {
  const question: ChatMessage = {
    id: "00000000-0000-4000-8000-000000000011",
    role: ROLES.User,
    name: "User",
    content: "try this again",
    createdAt: "2026-01-01T00:00:00.000Z",
  };
  const answer: ChatMessage = {
    id: "00000000-0000-4000-8000-000000000012",
    role: ROLES.Assistant,
    name: "Agent",
    content: "first answer",
    createdAt: "2026-01-01T00:00:01.000Z",
    questionId: question.id,
  };

  it("starts one streamed regeneration and skips the legacy endpoint", async () => {
    const history = makeHistory([question, answer]);
    const regenerateStreamedExecution = vi.fn(() =>
      Promise.resolve<StreamStartOutcome>({ started: true }),
    );
    const triggerRegenerate = vi.fn(() => Promise.resolve({}));
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [question, answer],
        regenerateStreamedExecution,
        triggerRegenerate,
      }),
    );

    await handlers.regenerateAnswer(answer.id);

    expect(regenerateStreamedExecution).toHaveBeenCalledWith({
      messageId: answer.id,
      questionId: question.id,
      question: question.content,
    });
    expect(triggerRegenerate).not.toHaveBeenCalled();
    expect(history.read()[1]).toMatchObject({
      id: answer.id,
      isLoading: true,
      isStreaming: true,
    });
  });

  it("falls back to the legacy endpoint when the current route is absent", async () => {
    const history = makeHistory([question, answer]);
    const triggerRegenerate = vi.fn(() => Promise.resolve({}));
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [question, answer],
        regenerateStreamedExecution: () => Promise.resolve(noTransport),
        triggerRegenerate,
      }),
    );

    await handlers.regenerateAnswer(answer.id);

    expect(triggerRegenerate).toHaveBeenCalledWith(
      expect.objectContaining({
        id: answer.id,
        message_id: answer.id,
        question_id: question.id,
      }),
    );
  });
});

/**
 * THE 409 THAT REACHES THE USER'S CLICK, on the route that actually raises it.
 *
 * `agent_regeneration_pending` exists on ONE route: the contract route
 * (`?execution_contract=agent.regenerate.v1`) that `regenerateStreamedExecution`
 * posts to. The REST trigger `triggerRegenerate` posts no contract —
 * `buildRegeneratePayload` has no such field — and elitea-main answers that 400
 * before admission runs, so a retry attached to the REST trigger alone can
 * never see this refusal: it is exercised only by a test that hands the REST
 * trigger a 409 the real route would never send it.
 *
 * These cases drive the streamed path instead, through the outcome the
 * transport really reports (`{started: false, reason: "retry-later"}`, produced
 * by `useChatStreamRunStarters`' `regenerateDetailed` from the 409 body).
 */
describe("regenerateAnswer — the streamed 409 is retried, not swallowed", () => {
  const question: ChatMessage = {
    id: "00000000-0000-4000-8000-000000000031",
    role: ROLES.User,
    name: "User",
    content: "try this again",
    createdAt: "2026-01-01T00:00:00.000Z",
  };
  const answer: ChatMessage = {
    id: "00000000-0000-4000-8000-000000000032",
    role: ROLES.Assistant,
    name: "Agent",
    content: "first answer",
    createdAt: "2026-01-01T00:00:01.000Z",
    questionId: question.id,
  };

  const pending: StreamStartOutcome = { started: false, reason: "retry-later" };
  const started: StreamStartOutcome = { started: true };

  /**
   * Deps for one regeneration click. Only the deps are built here — the hook
   * itself is called inside each `it`, because `react-hooks/rules-of-hooks`
   * (rightly) refuses a hook call from a plain lowercase helper.
   */
  const depsFor = (
    history: ReturnType<typeof makeHistory>,
    regenerateStreamedExecution: NonNullable<
      ChatBoxHandlerDeps["regenerateStreamedExecution"]
    >,
    triggerRegenerate: NonNullable<ChatBoxHandlerDeps["triggerRegenerate"]>,
  ): ChatBoxHandlerDeps =>
    makeDeps({
      setChatHistory: history.setChatHistory,
      chatHistory: [question, answer],
      regenerateStreamedExecution,
      triggerRegenerate,
    });

  /** Drive the click to completion, letting the retry ladder's timers run. */
  const settle = async (pending: Promise<void>): Promise<void> => {
    await vi.runAllTimersAsync();
    await pending;
  };

  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("retries a 409 that clears and runs the regeneration, leaving no failure on the card", async () => {
    const attempt = vi
      .fn<NonNullable<ChatBoxHandlerDeps["regenerateStreamedExecution"]>>()
      .mockResolvedValueOnce(pending)
      .mockResolvedValueOnce(started);
    const triggerRegenerate = vi.fn().mockResolvedValue({});

    const history = makeHistory([question, answer]);
    const handlers = useChatBoxHandlers(depsFor(history, attempt, triggerRegenerate));
    await settle(handlers.regenerateAnswer(answer.id));

    expect(attempt).toHaveBeenCalledTimes(2);
    // The run is live: the regenerating patch stands, the old answer is NOT
    // put back, and nothing is reported to the user.
    expect(history.read()[1]).toMatchObject({ id: answer.id, isStreaming: true });
    expect(history.read()[1]?.exception).toBeUndefined();
    // A started regeneration must never ALSO go out over the REST fallback:
    // that would run the agent twice against the same answer.
    expect(triggerRegenerate).not.toHaveBeenCalled();
  });

  it("gives up on a persistent 409, restores the answer and says why on the card", async () => {
    const attempt = vi
      .fn<NonNullable<ChatBoxHandlerDeps["regenerateStreamedExecution"]>>()
      .mockResolvedValue(pending);
    const triggerRegenerate = vi.fn().mockResolvedValue({});

    const history = makeHistory([question, answer]);
    const handlers = useChatBoxHandlers(depsFor(history, attempt, triggerRegenerate));
    await settle(handlers.regenerateAnswer(answer.id));

    // One attempt per delay in the ladder, plus the first.
    expect(attempt.mock.calls.length).toBeGreaterThan(1);
    expect(history.read()[1]).toMatchObject({ id: answer.id, content: answer.content });
    expect(history.read()[1]?.isStreaming).toBeFalsy();
    expect(String(history.read()[1]?.exception)).toBe(regenerationStillFinalizingText());
    // THE POINT OF THIS CASE. Falling through to the REST trigger would post
    // the same regeneration with no `execution_contract` and collect a 400 —
    // replacing the real reason with a wrong one.
    expect(triggerRegenerate).not.toHaveBeenCalled();
  });

  it("does NOT retry an absent transport — that one falls straight through to REST", async () => {
    const attempt = vi
      .fn<NonNullable<ChatBoxHandlerDeps["regenerateStreamedExecution"]>>()
      .mockResolvedValue({ started: false, reason: "no-transport" });
    const triggerRegenerate = vi.fn().mockResolvedValue({});

    const history = makeHistory([question, answer]);
    const handlers = useChatBoxHandlers(depsFor(history, attempt, triggerRegenerate));
    await settle(handlers.regenerateAnswer(answer.id));

    expect(attempt).toHaveBeenCalledTimes(1);
    expect(triggerRegenerate).toHaveBeenCalledTimes(1);
  });
});

describe("regenerateAnswer — the still-finalizing 409 retries", () => {
  const question: ChatMessage = {
    id: "00000000-0000-4000-8000-000000000021",
    role: ROLES.User,
    name: "User",
    content: "try this again",
    createdAt: "2026-01-01T00:00:00.000Z",
  };
  const answer: ChatMessage = {
    id: "00000000-0000-4000-8000-000000000022",
    role: ROLES.Assistant,
    name: "Agent",
    content: "first answer",
    createdAt: "2026-01-01T00:00:01.000Z",
    questionId: question.id,
  };

  /** The retryable 409 the Go route returns while `is_streaming` is still TRUE. */
  const regenerationPending = (): EliteaApiError =>
    new EliteaApiError({
      kind: "http",
      status: 409,
      url: "/elitea_core/regenerate/prompt_lib/1/x",
      body: { error: "agent_regeneration_pending", retryable: true },
    });

  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("retries the still-finalizing 409 and succeeds without restoring", async () => {
    const history = makeHistory([question, answer]);
    const triggerRegenerate = vi
      .fn()
      .mockRejectedValueOnce(regenerationPending())
      .mockResolvedValueOnce({});
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [question, answer],
        triggerRegenerate,
      }),
    );

    const pending = handlers.regenerateAnswer(answer.id);
    await vi.runAllTimersAsync();
    await pending;

    expect(triggerRegenerate).toHaveBeenCalledTimes(2);
    // Success ⇒ the streaming patch stays; the old answer is NOT restored.
    expect(history.read()[1]).toMatchObject({
      id: answer.id,
      isLoading: true,
      isStreaming: true,
    });
  });

  it("exhausts the budget on a persistent 409 and restores the old answer", async () => {
    const history = makeHistory([question, answer]);
    const triggerRegenerate = vi
      .fn()
      .mockRejectedValue(regenerationPending());
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [question, answer],
        triggerRegenerate,
      }),
    );

    const pending = handlers.regenerateAnswer(answer.id);
    await vi.runAllTimersAsync();
    await pending;

    // One initial attempt + three bounded retries.
    expect(triggerRegenerate).toHaveBeenCalledTimes(4);
    expect(history.read()[1]).toEqual(answer);
    expect(history.read()[1]).not.toMatchObject({ isStreaming: true });
  });

  it("restores immediately on a non-retryable error", async () => {
    const history = makeHistory([question, answer]);
    const triggerRegenerate = vi.fn().mockRejectedValue(
      new EliteaApiError({
        kind: "http",
        status: 500,
        url: "/elitea_core/regenerate/prompt_lib/1/x",
        body: { error: "boom" },
      }),
    );
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [question, answer],
        triggerRegenerate,
      }),
    );

    const pending = handlers.regenerateAnswer(answer.id);
    await vi.runAllTimersAsync();
    await pending;

    expect(triggerRegenerate).toHaveBeenCalledTimes(1);
    expect(history.read()[1]).toEqual(answer);
  });
});

const pausedMessage: ChatMessage = {
  id: "answer-1",
  role: ROLES.Assistant,
  name: "Agent",
  content: "",
  createdAt: "2026-01-01T00:00:00.000Z",
  questionId: "question-1",
  hitlInterrupt: { tool_call_id: "call-1" },
};

function authorizationAction(
  interruptId: string,
  toolCallId: string,
  storageKey = "cfg-1:https://login.example.test",
) {
  return {
    id: interruptId,
    authorizationRequestId: interruptId,
    name: "SharePoint search",
    status: "action_required",
    type: "toolkit",
    toolOutputs: { server_url: storageKey },
    toolMeta: {
      interrupt_id: interruptId,
      tool_call_id: toolCallId,
      server_url: "https://sharepoint.example.test",
      toolkit_type: "sharepoint",
    },
  };
}

function authorizationMessage(...actions: readonly ReturnType<typeof authorizationAction>[]): ChatMessage {
  return {
    ...pausedMessage,
    hitlInterrupt: undefined,
    threadId: "thread-auth-1",
    toolActions: actions,
  };
}

describe("continueHitl — a resume no transport accepted", () => {
  it("puts the approval card back and stops the spinner", async () => {
    const history = makeHistory([pausedMessage]);
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [pausedMessage],
        emitSocket: deadSocket(),
      }),
    );

    await handlers.continueHitl({ action: "approve" });

    const restored = history.read()[0];
    expect(restored?.hitlInterrupt).toEqual({ tool_call_id: "call-1" });
    expect(restored?.isLoading).toBe(false);
    expect(restored?.isStreaming).toBe(false);
    expect(String(restored?.exception)).toContain("was not sent");
  });

  it("keeps the optimistic patch when the emit really delivered", async () => {
    const history = makeHistory([pausedMessage]);
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [pausedMessage],
        emitSocket: () => true,
      }),
    );

    await handlers.continueHitl({ action: "approve" });

    const patched = history.read()[0];
    expect(patched?.hitlInterrupt).toBeUndefined();
    expect(patched?.isLoading).toBe(true);
    expect(patched?.exception).toBeUndefined();
  });
});

/**
 * DEFECT 3 — the resume never reached the backend that serves it.
 *
 * `continueHitl` emitted `chat_continue_predict` and nothing else, so on a
 * deployment with an empty `vite_socket_server` the approval went nowhere: the
 * run stayed paused server-side for good. The Go continuation route was
 * implemented, mounted and RBAC-gated the whole time, with no caller.
 */
interface ContinuationCall {
  readonly conversationUuid: string;
  readonly contract: string;
  readonly body: Record<string, unknown>;
}

/** Records every REST continuation and reports that the route accepted it. */
function captureContinuations(): {
  readonly calls: readonly ContinuationCall[];
  readonly continueStreamedExecution: NonNullable<
    ChatBoxHandlerDeps["continueStreamedExecution"]
  >;
} {
  const calls: ContinuationCall[] = [];
  return {
    calls,
    continueStreamedExecution: (params) => {
      calls.push(params);
      return Promise.resolve<StreamStartOutcome>({ started: true });
    },
  };
}

describe("continueHitl — the REST continuation", () => {
  it("POSTs the route body and does not also emit on the socket", async () => {
    const history = makeHistory([pausedMessage]);
    const emitSocket = vi.fn(() => true);
    const seen = captureContinuations();
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [pausedMessage],
        emitSocket,
        continueStreamedExecution: seen.continueStreamedExecution,
      }),
    );

    await handlers.continueHitl({ action: "edit", value: "do it differently" });

    expect(seen.calls).toHaveLength(1);
    // A second resume over the socket would run the agent twice.
    expect(emitSocket).not.toHaveBeenCalled();
    const call = seen.calls[0]!;
    expect(call.conversationUuid).toBe("conv-uuid-1");
    expect(call.contract).toBe("agent.continue.hitl.v1");
    // `project_id` is a NUMBER for this route; the socket payload sends a string.
    expect(call.body["project_id"]).toBe(1);
    expect(call.body["conversation_uuid"]).toBe("conv-uuid-1");
    expect(call.body["message_id"]).toBe("answer-1");
    expect(call.body["hitl_resume"]).toBe(true);
    expect(call.body["hitl_action"]).toBe("edit");
    expect(call.body["hitl_value"]).toBe("do it differently");
    // The contract refuses these three alongside a HITL resume.
    expect(call.body["mcp_tokens"]).toBeUndefined();
    expect(call.body["ignored_mcp_servers"]).toBeUndefined();
    expect(call.body["hitl_decisions"]).toBeUndefined();
  });

  it("sends a clarification answer as a STRUCTURED hitl_value, not as the encoded string", async () => {
    // `currentHITLValue` (agentexecution/route.go) admits a JSON object or a
    // JSON string for `answer` and canonicalises what it admitted; the worker
    // parses that text back with `AskUserRequest::format_answer` and renders
    // one line per answered question into the tool result the model reads.
    // Passing the card's ENCODED string through unchanged would still be
    // ADMITTED — as one JSON blob quoted at the model — so "the resume was
    // accepted" cannot tell the two apart. The decoded shape is the assertion.
    const history = makeHistory([pausedMessage]);
    const emitSocket = vi.fn(() => true);
    const seen = captureContinuations();
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [pausedMessage],
        emitSocket,
        continueStreamedExecution: seen.continueStreamedExecution,
      }),
    );

    await handlers.continueHitl({
      action: "answer",
      value: JSON.stringify({ environment: "Staging", traits: ["Safe", "Fast"] }),
      toolCallId: "call-1",
    });

    const call = seen.calls[0]!;
    expect(call.contract).toBe("agent.continue.hitl.v1");
    expect(call.body["hitl_action"]).toBe("answer");
    expect(call.body["hitl_value"]).toEqual({ environment: "Staging", traits: ["Safe", "Fast"] });
    // The root shape, not the decisions one: a single pause resumes with
    // `hitl_action`, and the route REFUSES both in one body.
    expect(call.body["hitl_decisions"]).toBeUndefined();
    expect(emitSocket).not.toHaveBeenCalled();
  });

  it("sends a free-text answer as a JSON string the route also admits", async () => {
    // The no-questions fallback. `currentHITLValue` refuses anything that is
    // neither an object nor a string for `answer`, and a bare unquoted word is
    // not valid JSON — so what travels is the string itself.
    const history = makeHistory([pausedMessage]);
    const seen = captureContinuations();
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [pausedMessage],
        continueStreamedExecution: seen.continueStreamedExecution,
      }),
    );

    await handlers.continueHitl({ action: "answer", value: JSON.stringify("Staging"), toolCallId: "call-1" });

    expect(seen.calls[0]!.body["hitl_value"]).toBe("Staging");
  });

  it("falls back to the socket when the route refuses the resume", async () => {
    const history = makeHistory([pausedMessage]);
    const emitSocket = vi.fn(() => true);
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [pausedMessage],
        emitSocket,
        continueStreamedExecution: () => Promise.resolve(noTransport),
      }),
    );

    await handlers.continueHitl({ action: "approve" });

    expect(emitSocket).toHaveBeenCalledTimes(1);
    expect(history.read()[0]?.exception).toBeUndefined();
  });

  // A fan-out child decision needs an `interrupt_id`. `currentHITLDecisions`
  // refuses an entry without one, so a pause that carries none must not be
  // POSTed at all.
  it("sends a fan-out decision with its interrupt_id", async () => {
    const fanout: ChatMessage = {
      ...pausedMessage,
      hitlInterrupt: undefined,
      hitlInterrupts: [{ interrupt_id: "int-9", tool_call_id: "call-1" }],
    };
    const history = makeHistory([fanout]);
    const seen = captureContinuations();
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [fanout],
        continueStreamedExecution: seen.continueStreamedExecution,
      }),
    );

    await handlers.continueHitl({
      action: "approve",
      toolCallId: "call-1",
      childThreadId: "thread-7",
    });

    const call = seen.calls[0]!;
    expect(call.body["thread_id"]).toBe("thread-7");
    expect(call.body["hitl_decisions"]).toEqual([
      {
        interrupt_id: "int-9",
        tool_call_id: "call-1",
        action: "approve",
        value: "",
      },
    ]);
    // `thread_id` inside a decision entry is refused by the route.
    expect(call.body["hitl_action"]).toBeUndefined();
  });

  it("stays on the socket for a fan-out decision with no interrupt_id", async () => {
    const fanout: ChatMessage = {
      ...pausedMessage,
      hitlInterrupt: undefined,
      hitlInterrupts: [{ tool_call_id: "call-1" }],
    };
    const history = makeHistory([fanout]);
    const emitSocket = vi.fn(() => true);
    const seen = captureContinuations();
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [fanout],
        emitSocket,
        continueStreamedExecution: seen.continueStreamedExecution,
      }),
    );

    await handlers.continueHitl({
      action: "approve",
      toolCallId: "call-1",
      childThreadId: "thread-7",
    });

    expect(seen.calls).toHaveLength(0);
    expect(emitSocket).toHaveBeenCalledTimes(1);
  });
});

describe("continueTokenLimit / resumeMcpFlow transport", () => {
  const tokenLimitMessage: ChatMessage = {
    ...pausedMessage,
    hitlInterrupt: undefined,
    requiresConfirmation: { message: "Continue?", buttonText: "Continue" },
  };

  it("continueTokenLimit POSTs the output-limit contract and keeps the existing answer", async () => {
    const history = makeHistory([tokenLimitMessage]);
    const seen = captureContinuations();
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [tokenLimitMessage],
        continueStreamedExecution: seen.continueStreamedExecution,
      }),
    );

    await handlers.continueTokenLimit("answer-1");

    expect(seen.calls).toEqual([
      {
        conversationUuid: "conv-uuid-1",
        contract: "agent.continue.output-limit.v1",
        body: {
          project_id: 1,
          conversation_uuid: "conv-uuid-1",
          message_id: "answer-1",
        },
      },
    ]);
    expect(history.read()[0]?.content).toBe(tokenLimitMessage.content);
    expect(history.read()[0]?.isStreaming).toBe(true);
  });

  it("continueTokenLimit reverts its own spinner", async () => {
    const history = makeHistory([tokenLimitMessage]);
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [tokenLimitMessage],
        emitSocket: deadSocket(),
      }),
    );

    await handlers.continueTokenLimit("answer-1");

    expect(history.read()[0]?.isLoading).toBe(false);
    expect(history.read()[0]?.exception).toBeDefined();
  });

  it("resumeMcpFlow reverts its own spinner", async () => {
    const message = authorizationMessage(authorizationAction("auth-1", "call-1"));
    const history = makeHistory([message]);
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: [message],
        emitSocket: deadSocket(),
      }),
    );

    await handlers.resumeMcpFlow("answer-1", false, "auth-1");

    expect(history.read()[0]?.isStreaming).toBe(false);
    expect(history.read()[0]?.exception).toBeDefined();
  });

  it("resumes one exact request with the OAuth token map", async () => {
    const message = authorizationMessage(authorizationAction("auth-1", "call-1"));
    const history = makeHistory([message]);
    const seen = captureContinuations();
    const emitSocket = vi.fn(() => true);
    const handlers = useChatBoxHandlers(makeDeps({
      setChatHistory: history.setChatHistory,
      chatHistory: [message],
      emitSocket,
      getMcpTokens: () => ({
        "cfg-1:https://login.example.test": { access_token: "runtime-token" },
        "unrelated:https://login.example.test": { access_token: "unrelated-token" },
      }),
      continueStreamedExecution: seen.continueStreamedExecution,
    }));

    await handlers.resumeMcpFlow("answer-1", false, "auth-1");

    expect(emitSocket).not.toHaveBeenCalled();
    expect(seen.calls).toHaveLength(1);
    expect(seen.calls[0]).toMatchObject({
      conversationUuid: "conv-uuid-1",
      contract: "agent.continue.authorization.v1",
      body: {
        project_id: 1,
        authorization_request_id: "auth-1",
        authorization_action: "authorize",
        hitl_resume: false,
        mcp_tokens: { "cfg-1:https://login.example.test": { access_token: "runtime-token" } },
      },
    });
  });

  it("applies one authorization to parallel requests sharing the same credential", async () => {
    const message = authorizationMessage(
      authorizationAction("auth-1", "call-1"),
      authorizationAction("auth-2", "call-2"),
    );
    const history = makeHistory([message]);
    const seen = captureContinuations();
    const handlers = useChatBoxHandlers(makeDeps({
      setChatHistory: history.setChatHistory,
      chatHistory: [message],
      sessionMcpAuthorizationBatchesRef: { current: new Map() },
      getMcpTokens: () => ({ "cfg-1:https://login.example.test": { access_token: "runtime-token" } }),
      continueStreamedExecution: seen.continueStreamedExecution,
    }));

    await handlers.resumeMcpFlow("answer-1", false, "auth-1");

    expect(seen.calls).toHaveLength(1);
    expect(seen.calls[0]?.body).toMatchObject({
      hitl_resume: true,
      authorization_request_id: "",
      authorization_action: "",
      hitl_decisions: [
        { interrupt_id: "auth-1", tool_call_id: "call-1", guardrail_type: "mcp_auth", action: "authorize" },
        { interrupt_id: "auth-2", tool_call_id: "call-2", guardrail_type: "mcp_auth", action: "authorize" },
      ],
    });
  });

  it("waits for separate OAuth groups and then sends one complete decision set", async () => {
    const message = authorizationMessage(
      authorizationAction("auth-1", "call-1", "cfg-1:https://login.example.test"),
      authorizationAction("auth-2", "call-2", "cfg-2:https://login.example.test"),
    );
    const history = makeHistory([message]);
    const seen = captureContinuations();
    const batches = { current: new Map() };
    const first = useChatBoxHandlers(makeDeps({
      setChatHistory: history.setChatHistory,
      chatHistory: [message],
      sessionMcpAuthorizationBatchesRef: batches,
      continueStreamedExecution: seen.continueStreamedExecution,
    }));

    await first.resumeMcpFlow("answer-1", false, "auth-1");
    expect(seen.calls).toHaveLength(0);
    expect(history.read()[0]?.toolActions).toHaveLength(1);

    const second = useChatBoxHandlers(makeDeps({
      setChatHistory: history.setChatHistory,
      chatHistory: history.read(),
      sessionMcpAuthorizationBatchesRef: batches,
      continueStreamedExecution: seen.continueStreamedExecution,
    }));
    await second.resumeMcpFlow("answer-1", true, "auth-2");

    expect(seen.calls[0]?.body['hitl_decisions']).toEqual([
      { interrupt_id: "auth-1", tool_call_id: "call-1", guardrail_type: "mcp_auth", action: "authorize" },
      { interrupt_id: "auth-2", tool_call_id: "call-2", guardrail_type: "mcp_auth", action: "skip" },
    ]);
  });
});

/**
 * `deleteAnswer` — the ONLY delete path that makes a real network call, and
 * until now the only one with no test at all.
 *
 * The server deletes an answer together with the question it replies to and
 * names both groups in its response. This handler used to filter on the single
 * id it passed in, so the paired question stayed rendered until the next
 * refetch — and nothing failed to compile when the contract changed, because
 * `triggerDeleteMessage` was typed `Promise<unknown>`.
 */
describe("useChatBoxHandlers — deleteAnswer", () => {
  const seeded: readonly ChatMessage[] = [
    { id: "question", role: ROLES.User, content: "why?" } as ChatMessage,
    { id: "answer", role: ROLES.Assistant, content: "because" } as ChatMessage,
    { id: "keep", role: ROLES.Assistant, content: "unrelated" } as ChatMessage,
  ];

  it("prunes every group the server reports it deleted", async () => {
    const history = makeHistory(seeded);
    const triggerDeleteMessage = vi
      .fn()
      .mockResolvedValue({ deleted: ["answer", "question"] });
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: seeded,
        triggerDeleteMessage,
      }),
    );

    await handlers.deleteAnswer("answer");

    expect(triggerDeleteMessage).toHaveBeenCalledWith(
      expect.objectContaining({ id: "answer" }),
    );
    expect(history.read().map((item) => item.id)).toEqual(["keep"]);
  });

  // A server that still answers 204, or omits the field, must not make the
  // handler prune nothing: the id it named certainly went.
  it.each([
    ["an empty deleted list", { deleted: [] }],
    ["a body with no deleted field", {}],
    ["no body at all", undefined],
  ])("falls back to the requested id given %s", async (_label, resolved) => {
    const history = makeHistory(seeded);
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: seeded,
        triggerDeleteMessage: vi.fn().mockResolvedValue(resolved),
      }),
    );

    await handlers.deleteAnswer("answer");

    expect(history.read().map((item) => item.id)).toEqual(["question", "keep"]);
  });

  // A failed delete must leave the transcript alone. Pruning optimistically
  // would hide a message the server still holds.
  it("leaves the history untouched when the delete fails", async () => {
    const history = makeHistory(seeded);
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        chatHistory: seeded,
        triggerDeleteMessage: vi.fn().mockRejectedValue(new Error("nope")),
      }),
    );

    await handlers.deleteAnswer("answer");

    expect(history.read().map((item) => item.id)).toEqual([
      "question",
      "answer",
      "keep",
    ]);
  });
});

/**
 * WHICH IDENTIFIER THE ATTACHMENT IS UPLOADED UNDER — asserted through the
 * handler, not through `resolveUploadConversationId` alone.
 *
 * The pure resolver has its own tests next door; this one exists because the
 * defect was in the WIRING. The resolver was handed `deps.conversationId`,
 * both halves were internally consistent, and every unit test passed while the
 * composer stored every attachment under a key admission would refuse (400,
 * with the user's question lost before `admissions.Submit` ever ran).
 */
describe("sendQuestion — attachments are uploaded under the conversation UUID", () => {
  const started: StreamStartOutcome = { started: true };

  it("uploads to the EXISTING conversation's uuid, never its numeric id", async () => {
    const history = makeHistory([]);
    const uploadAttachments = vi.fn().mockResolvedValue({
      success: true,
      uploaded: [{ filepath: "/chat-attachments/conv-uuid-1/a.txt", sanitizedName: "a.txt" }],
    });
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        conversationUuid: "conv-uuid-1",
        conversationId: 77,
        uploadAttachments,
        startStreamedExecution: () => Promise.resolve(started),
      }),
    );

    const file = new File(["x"], "a.txt");
    await handlers.sendQuestion({ question: "hi", attachments: [file] });

    expect(uploadAttachments).toHaveBeenCalledWith("conv-uuid-1", [file]);
    expect(uploadAttachments).not.toHaveBeenCalledWith(77, [file]);
  });

  it("uploads to the uuid of a conversation this very send created", async () => {
    const history = makeHistory([]);
    const uploadAttachments = vi.fn().mockResolvedValue({
      success: true,
      uploaded: [{ filepath: "/chat-attachments/created-uuid/a.txt", sanitizedName: "a.txt" }],
    });
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        // No conversation yet — the send creates one, and the created row is
        // the ONLY source of its uuid. This is the path a first attachment
        // takes, and the one the old resolver keyed by `id`.
        conversationUuid: undefined,
        conversationId: undefined,
        createConversation: () => Promise.resolve({ id: 501, uuid: "created-uuid" }),
        uploadAttachments,
        startStreamedExecution: () => Promise.resolve(started),
      }),
    );

    const file = new File(["x"], "a.txt");
    const result = await handlers.sendQuestion({ question: "hi", attachments: [file] });

    expect(uploadAttachments).toHaveBeenCalledWith("created-uuid", [file]);
    expect(uploadAttachments).not.toHaveBeenCalledWith(501, [file]);
    expect(result.success).toBe(true);
  });

  /**
   * The uploaded entries — `{filepath, name}` — are what the start body's
   * `payload.attachments` carries, and the filepath is what admission splits
   * to recover the object key. A send that uploaded correctly and then sent
   * nothing would fail in exactly the same place, so the payload is asserted
   * too.
   */
  it("threads the uploaded entries into the turn's payload", async () => {
    const history = makeHistory([]);
    const startStreamedExecution = vi.fn().mockResolvedValue(started);
    const handlers = useChatBoxHandlers(
      makeDeps({
        setChatHistory: history.setChatHistory,
        conversationUuid: "conv-uuid-1",
        uploadAttachments: () =>
          Promise.resolve({
            success: true,
            uploaded: [{ filepath: "/chat-attachments/conv-uuid-1/a.txt", sanitizedName: "a.txt" }],
          }),
        startStreamedExecution,
      }),
    );

    await handlers.sendQuestion({ question: "hi", attachments: [new File(["x"], "a.txt")] });

    const params = startStreamedExecution.mock.calls[0]?.[0] as {
      readonly payload: Record<string, unknown>;
    };
    expect(params.payload["attachments"]).toEqual([
      { filepath: "/chat-attachments/conv-uuid-1/a.txt", name: "a.txt" },
    ]);
  });
});
