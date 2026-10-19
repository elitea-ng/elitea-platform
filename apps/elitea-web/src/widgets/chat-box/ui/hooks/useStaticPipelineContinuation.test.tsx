import { useState } from "react";
import {
  act,
  fireEvent,
  renderHook,
  screen,
  waitFor,
} from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { staticPauseBinding } from "@/features/chat-messages/lib/staticPipelinePause";
import type { ChatMessage } from "@/features/chat-messages";
import {
  useStaticPipelineContinuation,
  type StaticContinuationParams,
} from "./useStaticPipelineContinuation";
import { useStaticPauseCards } from "../StaticPipelineContinueControls";
import { StaticPauseCard } from "@/features/chat-messages/ui/chat-box/StaticPauseCard";
import { renderWithTheme } from "@/shared/ui/lib/testTheme";
import type { StreamStartOutcome } from "./useChatBoxHandlers.helpers";

const response = "30e0913e-10d4-43db-b8d0-c7b79480935a",
  generation = "ee92ccbd-3312-4c72-b20b-fddf224e7c0e";
const proof = (letter = "a", kind: "before" | "after" = "before") => ({
  revision: 1,
  pause_id: `pipeline-static:sha256:${letter.repeat(64)}`,
  checkpoint_id: "original",
  kind,
  node_name: "tick",
  definition_digest: `sha256:${"b".repeat(64)}`,
  node_digest: `sha256:${"c".repeat(64)}`,
  pending_nodes: kind === "before" ? ["tick"] : [],
  step: 2,
  descendant_path: [],
});
const leaf = (id: string, letter: string) => ({
  tool_call_id: "call-" + id,
  child_thread_id: "child-" + id,
  original_batch_event_id: "batch-" + id,
  original_ordinal: 1,
  proof: proof(letter),
});
const pause = staticPauseBinding(response, generation, "root", undefined, {
  revision: 1,
  pauses: [leaf("one", "a"), leaf("two", "d")],
})!;
const message: ChatMessage = {
  id: response,
  role: "assistant",
  name: "Agent",
  content: "original",
  createdAt: "2026-10-02T00:00:00Z",
  executionGeneration: generation,
  threadId: "root",
  staticPause: pause,
  hitlInterrupt: { interrupt_id: "untouched-dynamic" },
};
function base(
  continueExecution: StaticContinuationParams["continueExecution"],
): StaticContinuationParams {
  return {
    messages: [message],
    setChatHistory: () => {},
    projectId: 7,
    conversationUuid: generation,
    continueExecution,
    isStreaming: false,
  };
}

describe("static Continue controls", () => {
  it("restores controls without work and ignores mismatched Test scope", () => {
    let calls = 0;
    const params = base(() => {
      calls++;
      return Promise.resolve({ started: true });
    });
    const { result, rerender } = renderHook(
      (p) => useStaticPipelineContinuation(p),
      { initialProps: params },
    );
    expect(result.current.pause).toBeDefined();
    expect(calls).toBe(0);
    const run = {
      response_message_id: response,
      question_id: generation,
      execution_id: "original-execution",
      execution_generation: generation,
      phase: "PAUSED" as const,
      state: "PAUSED",
      desired_state: "RUN",
      admitted_at: "2026-10-02T00:00:00Z",
      input_reference: {
        bundle_id: "bundle",
        entry_id: "entry",
        immutable_version: "version",
        content_digest: "sha256:" + "e".repeat(64),
      },
      can_control: true,
    };
    rerender({
      ...params,
      restoredRun: { run, projectId: 7, conversationUuid: "other" },
    });
    expect(result.current.pause).toBeUndefined();
    expect(calls).toBe(0);
  });
  it("continues one leaf on its own card once and keeps its siblings and dynamic cards", async () => {
    const calls: Parameters<
      StaticContinuationParams["continueExecution"]
    >[0][] = [];
    let complete: (outcome: StreamStartOutcome) => void = () => {};
    const continuation = new Promise<StreamStartOutcome>((resolve) => {
      complete = resolve;
    });
    function Host() {
      const [messages, setChatHistory] = useState<readonly ChatMessage[]>([
        message,
      ]);
      const binding = useStaticPauseCards({
        ...base((request) => {
          calls.push(request);
          return continuation;
        }),
        messages,
        setChatHistory,
        editorTest: undefined,
      });
      return (
        <>
          <span data-testid="dynamic">
            {messages[0]?.hitlInterrupt ? "dynamic remains" : "missing"}
          </span>
          {binding?.pause.kind === "tools" &&
            binding.pause.inventory.pauses.map((leafPause) => (
              <StaticPauseCard
                key={leafPause.proof.pause_id}
                nodeName={leafPause.proof.node_name}
                disabled={binding.busy}
                onContinue={() => binding.onContinue(leafPause.proof.pause_id)}
              />
            ))}
        </>
      );
    }
    renderWithTheme(<Host />);
    expect(calls).toHaveLength(0);
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("checkbox")).toBeNull();
    const second = screen.getAllByTestId("chat-static-pause-continue-button")[1]!;
    fireEvent.click(second);
    fireEvent.click(second);
    expect(calls).toHaveLength(1);
    expect(calls[0]).toMatchObject({
      conversationUuid: generation,
      contract: "agent.continue.static.v1",
      body: {
        message_id: response,
        static_decisions: [
          {
            pause_id: leaf("two", "d").proof.pause_id,
            child_thread_id: "child-two",
            tool_call_id: "call-two",
            action: "continue",
            value: "Continue",
          },
        ],
      },
    });
    await act(async () => {
      complete({ started: true });
      await continuation;
    });
    await waitFor(() =>
      expect(screen.getAllByTestId("chat-static-pause-panel")).toHaveLength(1),
    );
    expect(screen.getByTestId("dynamic")).toHaveTextContent("dynamic remains");
  });
  it("refuses a stale closure after generation/context changes", async () => {
    let calls = 0;
    const params = base(() => {
      calls++;
      return Promise.resolve({ started: true });
    });
    const { result, rerender } = renderHook(
      (p) => useStaticPipelineContinuation(p),
      { initialProps: params },
    );
    const stale = result.current.submit;
    rerender({
      ...params,
      conversationUuid: "ff92ccbd-3312-4c72-b20b-fddf224e7c0e",
    });
    await act(async () => stale("Continue", [leaf("one", "a").proof.pause_id]));
    expect(calls).toBe(0);
    const old = result.current.submit;
    rerender({
      ...params,
      messages: [{ ...message, executionGeneration: "different" }],
    });
    await act(async () => old("Continue", [leaf("one", "a").proof.pause_id]));
    expect(calls).toBe(0);
  });
  it("clears refused controls without a second admission or dynamic mutation", async () => {
    let calls = 0;
    const { result } = renderHook(() => {
      const [messages, setChatHistory] = useState<readonly ChatMessage[]>([
        message,
      ]);
      const controls = useStaticPipelineContinuation({
        ...base(() => {
          calls++;
          return Promise.resolve({
            started: false,
            reason: "rejected",
            message: "Original pause is stale.",
          });
        }),
        messages,
        setChatHistory,
      });
      return { controls, messages };
    });
    await act(async () =>
      result.current.controls.submit("Continue", [
        leaf("one", "a").proof.pause_id,
      ]),
    );
    expect(calls).toBe(1);
    expect(result.current.messages[0]?.staticPause).toBeUndefined();
    expect(result.current.messages[0]?.hitlInterrupt).toBe(
      message.hitlInterrupt,
    );
  });
  it.each(["before", "after"] as const)(
    "continues a %s root pause with the fixed text and no selection",
    async (kind) => {
      const root = staticPauseBinding(
        response,
        generation,
        "root",
        proof("a", kind),
        undefined,
      )!;
      const calls: Parameters<StaticContinuationParams["continueExecution"]>[0][] = [];
      const { result } = renderHook(() =>
        useStaticPauseCards({
          ...base((request) => {
            calls.push(request);
            return Promise.resolve({ started: true });
          }),
          messages: [{ ...message, staticPause: root }],
          editorTest: undefined,
        }),
      );
      await act(async () => {
        result.current?.onContinue(undefined);
        await Promise.resolve();
      });
      expect(calls[0]?.body).toMatchObject({
        static_pause_id: proof("a", kind).pause_id,
        user_input: "Continue",
      });
    },
  );
});
