/**
 * useChatStreamConnection.test.ts — the connection lifecycle on its own.
 *
 * `useChatStreamTransport.test.tsx` pins this machinery as the chat surface
 * uses it (a real POST, a real recorded run, chat history at the end). These
 * cases pin it as a MODULE: given a URL and three callbacks, what does it open,
 * what does it reopen, and when does it stop — with no transport, no reducer
 * and no history in the picture. That separation is the point of the split: a
 * future caller of this hook (a second surface with its own reducer) inherits
 * the #329 resume contract, and this is where that contract is stated.
 */
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { resetConfigForTests } from "@/shared/config/get-config";
import {
  installTestEventSource,
  type TestEventSourceRegistry,
} from "@/shared/api/sse/testing";
import type { ExecutionEventData } from "@/shared/api/sse";

import { useChatStreamConnection } from "./useChatStreamConnection";

const BASE = "/api/v2";
const EVENTS_URL = "/api/v2/executions/7/exec-1/events";
const CONNECTION_LOST = "Connection interrupted. Reconnecting to the existing run.";

const globals = globalThis as unknown as Record<string, unknown>;
let registry: TestEventSourceRegistry;

/** The three callbacks the transport supplies, captured for assertions. */
function handlers(): {
  readonly nodeEvents: ExecutionEventData[];
  readonly failures: ExecutionEventData[];
  readonly lost: string[];
  readonly onNodeEvent: (frame: ExecutionEventData) => void;
  readonly onFailed: (frame: ExecutionEventData) => void;
  readonly onConnectionInterrupted: (reason: string) => void;
} {
  const nodeEvents: ExecutionEventData[] = [];
  const failures: ExecutionEventData[] = [];
  const lost: string[] = [];
  return {
    nodeEvents,
    failures,
    lost,
    onNodeEvent: (frame) => nodeEvents.push(frame),
    onFailed: (frame) => failures.push(frame),
    onConnectionInterrupted: (reason) => lost.push(reason),
  };
}

beforeEach(() => {
  registry = installTestEventSource();
  globals["elitea_ui_config"] = {
    vite_server_url: BASE,
    vite_base_uri: "/",
    vite_public_project_id: "public-1",
  };
  resetConfigForTests();
  vi.useFakeTimers({ shouldAdvanceTime: true });
});

afterEach(() => {
  vi.useRealTimers();
  registry.restore();
  delete globals["elitea_ui_config"];
  resetConfigForTests();
});

describe("useChatStreamConnection", () => {
  it("subscribes to the URL it was opened with and reports it as streaming", () => {
    const spies = handlers();
    const { result } = renderHook(() => useChatStreamConnection(spies));
    expect(result.current.isStreaming).toBe(false);

    act(() => {
      result.current.open(EVENTS_URL);
    });

    expect(result.current.isStreaming).toBe(true);
    expect(registry.getOpen()).toHaveLength(1);
    expect(registry.getOpen()[0]?.url).toContain(EVENTS_URL);
    // Session-cookie auth: `EventSource` can carry no Authorization header.
    expect(registry.getOpen()[0]?.withCredentials).toBe(true);
  });

  it("hands progress and runtime-failure frames to the caller, unparsed of meaning", () => {
    const spies = handlers();
    const { result } = renderHook(() => useChatStreamConnection(spies));
    act(() => {
      result.current.open(EVENTS_URL);
    });

    act(() => {
      registry.emit(
        "execution.node_event",
        JSON.stringify({ type: "agent_llm_chunk", content: "hi" }),
        "11",
      );
      registry.emit(
        "execution.failed",
        JSON.stringify({ code: "INTERNAL", safe_message: "model unavailable" }),
        "12",
      );
    });

    expect(spies.nodeEvents).toEqual([
      { type: "agent_llm_chunk", content: "hi" },
    ]);
    expect(spies.failures).toEqual([
      { code: "INTERNAL", safe_message: "model unavailable" },
    ]);
    // Nothing was decided about the turn here: ending it is the transport's
    // call, and this hook is still subscribed.
    expect(result.current.isStreaming).toBe(true);
  });

  it("reopens at the last cursor after a drop, and stays streaming across the gap", () => {
    const spies = handlers();
    const { result } = renderHook(() => useChatStreamConnection(spies));
    act(() => {
      result.current.open(EVENTS_URL);
    });
    act(() => {
      registry.emit("execution.node_event", JSON.stringify({ type: "x" }), "12");
    });

    act(() => {
      registry.fail();
      vi.advanceTimersByTime(999);
    });
    // The composer must not swap Stop back for Send here — the run is alive.
    expect(result.current.isStreaming).toBe(true);
    expect(registry.getOpen()).toHaveLength(0);

    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(registry.getOpen()).toHaveLength(1);
    // `cursor` is what `events.go`'s `requestedCursor` reads as
    // `Last-Event-ID`, so only frames after id 12 are replayed.
    expect(registry.getOpen()[0]?.url).toContain(`${EVENTS_URL}?cursor=12`);
    expect(spies.lost).toEqual([]);
  });

  it("keeps observing after the short reconnect burst and reports the outage once", () => {
    const spies = handlers();
    const { result } = renderHook(() => useChatStreamConnection(spies));
    act(() => {
      result.current.open(EVENTS_URL);
    });

    // The drop and the backoff have to be committed separately: the reopen
    // re-runs the subscription effect by first clearing the URL, and a single
    // `act` batches that render away — the same reason the transport test
    // advances its clock in a step of its own.
    for (const delay of [1_000, 2_000, 4_000, 8_000]) {
      act(() => {
        registry.fail();
      });
      act(() => {
        vi.advanceTimersByTime(delay);
      });
    }
    expect(registry.getSources()).toHaveLength(5);

    act(() => { registry.fail(); });
    act(() => { vi.advanceTimersByTime(29_999); });
    expect(registry.getSources()).toHaveLength(5);
    act(() => { vi.advanceTimersByTime(1); });
    expect(registry.getSources()).toHaveLength(6);
    act(() => { registry.fail(); });
    act(() => { vi.advanceTimersByTime(30_000); });
    expect(registry.getSources()).toHaveLength(7);
    expect(spies.lost).toEqual([CONNECTION_LOST]);
    expect(result.current.isStreaming).toBe(true);
    act(() => { result.current.close(); });
    act(() => { vi.advanceTimersByTime(600_000); });
    expect(registry.getOpen()).toHaveLength(0);
    expect(registry.getSources()).toHaveLength(7);
  });

  it("spends a fresh budget after a delivered frame", () => {
    const spies = handlers();
    const { result } = renderHook(() => useChatStreamConnection(spies));
    act(() => {
      result.current.open(EVENTS_URL);
    });

    for (const delay of [1_000, 2_000, 4_000, 8_000]) {
      act(() => {
        registry.fail();
      });
      act(() => {
        vi.advanceTimersByTime(delay);
      });
    }
    act(() => {
      registry.emit("execution.node_event", JSON.stringify({ type: "x" }), "7");
    });

    act(() => {
      registry.fail();
      vi.advanceTimersByTime(1_000);
    });

    expect(registry.getOpen()).toHaveLength(1);
    expect(registry.getOpen()[0]?.url).toContain("cursor=7");
    expect(spies.lost).toEqual([]);
  });

  it("stops reconnecting once it is closed, including a reopen already scheduled", () => {
    const spies = handlers();
    const { result } = renderHook(() => useChatStreamConnection(spies));
    act(() => {
      result.current.open(EVENTS_URL);
    });

    act(() => {
      registry.fail();
      // The drop scheduled a reopen; closing has to cancel it, not merely
      // close the socket that is already dead.
      result.current.close();
      vi.advanceTimersByTime(600_000);
    });

    expect(result.current.isStreaming).toBe(false);
    expect(registry.getSources()).toHaveLength(1);
    expect(registry.getOpen()).toHaveLength(0);
    expect(spies.lost).toEqual([]);
  });

  it("gives the next run a clean cursor and a clean budget", () => {
    // A second turn in the same conversation reuses this hook. Inheriting the
    // previous run's cursor would ask the server to replay a DIFFERENT
    // execution from an id that means nothing in it.
    const spies = handlers();
    const { result } = renderHook(() => useChatStreamConnection(spies));
    act(() => {
      result.current.open(EVENTS_URL);
    });
    act(() => {
      registry.emit("execution.node_event", JSON.stringify({ type: "x" }), "42");
    });
    act(() => {
      result.current.close();
    });

    act(() => {
      result.current.open("/api/v2/executions/7/exec-2/events");
    });
    act(() => {
      registry.fail();
      vi.advanceTimersByTime(1_000);
    });

    const reopened = registry.getOpen()[0]?.url ?? "";
    expect(reopened).toContain("/executions/7/exec-2/events");
    expect(reopened).not.toContain("cursor");
  });
});
