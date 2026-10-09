import { useState } from "react";
import { act, renderHook, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
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
import { useChatStreamTransport } from "./useChatStreamTransport";
import {
  staticPauseBinding,
  staticContinuationBody,
} from "../lib/staticPipelinePause";
import type { ChatMessage } from "../lib/convertMessagesToChatHistory.types";

const BASE = "/api/v2",
  response = "30e0913e-10d4-43db-b8d0-c7b79480935a",
  generation = "ee92ccbd-3312-4c72-b20b-fddf224e7c0e";
const EVENTS = "/api/v2/executions/7/static-execution/events";
const proof = {
  revision: 1,
  pause_id: "pipeline-static:sha256:" + "a".repeat(64),
  checkpoint_id: "checkpoint",
  kind: "before",
  node_name: "tick",
  definition_digest: "sha256:" + "b".repeat(64),
  node_digest: "sha256:" + "c".repeat(64),
  pending_nodes: ["tick"],
  step: 3,
  descendant_path: [],
};
const initial: ChatMessage = {
  id: response,
  role: "assistant",
  name: "Pipeline",
  content: "original",
  createdAt: "2026-10-02T00:00:00Z",
  executionGeneration: generation,
  threadId: "root",
  staticPause: staticPauseBinding(
    response,
    generation,
    "root",
    proof,
    undefined,
  ),
};
let registry: TestEventSourceRegistry;
beforeEach(() => {
  registry = installTestEventSource();
  (globalThis as unknown as Record<string, unknown>)["elitea_ui_config"] = {
    vite_server_url: BASE,
    vite_base_uri: "/",
    vite_public_project_id: "public-1",
  };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  registry.restore();
  delete (globalThis as unknown as Record<string, unknown>)["elitea_ui_config"];
  resetConfigForTests();
  resetGeneratedClient();
});
function useHarness() {
  const [history, setChatHistory] = useState<readonly ChatMessage[]>([initial]);
  const transport = useChatStreamTransport({
    setChatHistory,
    conversationUuid: generation,
  });
  return { transport, history };
}
const request = () => ({
  projectId: 7,
  conversationUuid: generation,
  contract: "agent.continue.static.v1",
  body: staticContinuationBody(
    initial.staticPause!,
    7,
    generation,
    "Continue",
    [],
  ),
});

describe("static durable transport", () => {
  it("uses the generated endpoint and settles the stream only on validated root static metadata", async () => {
    const bodies: unknown[] = [];
    server.use(
      http.post(
        `${BASE}/elitea_core/continue_predict/prompt_lib/7/${generation}`,
        async ({ request }) => {
          expect(
            new URL(request.url).searchParams.get("execution_contract"),
          ).toBe("agent.continue.static.v1");
          bodies.push(await request.json());
          return HttpResponse.json({
            task_id: "static-execution",
            execution_id: "static-execution",
            command_id: "command",
            response_message_id: response,
            events_url: EVENTS,
            created: true,
          });
        },
      ),
    );
    const { result } = renderHook(useHarness);
    await act(async () => {
      expect(await result.current.transport.resumeDetailed(request())).toEqual({
        started: true,
      });
    });
    expect(bodies).toEqual([request().body]);
    await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
    await act(() =>
      registry.emit(
        "execution.node_event",
        JSON.stringify({
          type: "agent_start",
          message_id: response,
          execution_generation: generation,
          response_metadata: { thread_id: "root" },
        }),
      ),
    );
    await act(() =>
      registry.emit(
        "execution.node_event",
        JSON.stringify({
          type: "full_message",
          message_id: response,
          execution_generation: generation,
          response_metadata: { thread_id: "root" },
        }),
      ),
    );
    expect(registry.getOpen()).toHaveLength(1);
    await act(() =>
      registry.emit(
        "execution.node_event",
        JSON.stringify({
          type: "full_message",
          message_id: response,
          execution_generation: generation,
          response_metadata: {
            thread_id: "root",
            application_details: { agent_type: "pipeline" },
            pipeline_static_v1: proof,
          },
        }),
      ),
    );
    await waitFor(() => expect(registry.getOpen()).toHaveLength(0));
    expect(result.current.transport.isStreaming).toBe(false);
    expect(result.current.history[0]?.staticPause?.messageId).toBe(response);
    expect(result.current.history[0]?.hitlInterrupt).toBeUndefined();
  });
  it("refuses mismatched response identity without opening its stream", async () => {
    let calls = 0;
    server.use(
      http.post(
        `${BASE}/elitea_core/continue_predict/prompt_lib/7/${generation}`,
        () => {
          calls++;
          return HttpResponse.json({
            task_id: "different",
            execution_id: "different",
            command_id: "command",
            response_message_id: generation,
            events_url: EVENTS,
            created: true,
          });
        },
      ),
    );
    const { result } = renderHook(useHarness);
    await act(async () =>
      expect(
        await result.current.transport.resumeDetailed(request()),
      ).toMatchObject({ started: false, reason: "rejected" }),
    );
    expect(calls).toBe(1);
    expect(registry.getOpen()).toHaveLength(0);
  });
  it("rejects checkpoint selectors before making any request", async () => {
    let calls = 0;
    server.use(
      http.post(
        `${BASE}/elitea_core/continue_predict/prompt_lib/7/${generation}`,
        () => {
          calls++;
          return HttpResponse.json({});
        },
      ),
    );
    const { result } = renderHook(useHarness);
    const invalid = {
      ...request(),
      body: { ...request().body, checkpoint_id: "forbidden" },
    };
    await act(async () =>
      expect(
        await result.current.transport.resumeDetailed(invalid),
      ).toMatchObject({ started: false, reason: "rejected" }),
    );
    expect(calls).toBe(0);
  });
  it.each([409, 422])(
    "returns a %s refusal without opening a stream or resubmitting",
    async (status) => {
      let calls = 0;
      server.use(
        http.post(
          `${BASE}/elitea_core/continue_predict/prompt_lib/7/${generation}`,
          () => {
            calls++;
            return HttpResponse.json(
              {
                error: "static_pause_not_current",
                safe_message: "The original pause is no longer current.",
              },
              { status },
            );
          },
        ),
      );
      const { result } = renderHook(useHarness);
      await act(async () => {
        expect(
          await result.current.transport.resumeDetailed(request()),
        ).toMatchObject({ started: false, reason: "rejected" });
      });
      expect(calls).toBe(1);
      expect(registry.getOpen()).toHaveLength(0);
    },
  );
});
