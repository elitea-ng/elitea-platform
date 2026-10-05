import { describe, expect, it } from "vitest";
import { applyChatStreamFrame } from "./chatStreamReducer";
import {
  consumeStaticPause,
  currentStaticPause,
  staticContinuationBody,
  staticPauseBinding,
} from "./staticPipelinePause";
import { convertMessagesToChatHistory } from "./convertMessagesToChatHistory";
import type { ChatMessage } from "./convertMessagesToChatHistory.types";

const response = "30e0913e-10d4-43db-b8d0-c7b79480935a",
  generation = "ee92ccbd-3312-4c72-b20b-fddf224e7c0e";
const proof = (letter = "a", kind: "before" | "after" = "before") => ({
  revision: 1,
  pause_id: `pipeline-static:sha256:${letter.repeat(64)}`,
  checkpoint_id: "original-checkpoint",
  kind,
  node_name: "tick",
  definition_digest: `sha256:${"b".repeat(64)}`,
  node_digest: `sha256:${"c".repeat(64)}`,
  pending_nodes: kind === "before" ? ["tick"] : ["next"],
  step: 7,
  descendant_path: [],
});
const inventory = () => ({
  revision: 1,
  pauses: [
    {
      tool_call_id: "same-call",
      child_thread_id: "child-1",
      original_batch_event_id: "original-batch-1",
      original_ordinal: 16,
      proof: proof("a"),
    },
    {
      tool_call_id: "same-call",
      child_thread_id: "child-2",
      original_batch_event_id: "original-batch-2",
      original_ordinal: 16,
      proof: proof("d", "after"),
    },
  ],
});
const message = (
  binding = staticPauseBinding(
    response,
    generation,
    "root",
    proof(),
    undefined,
  ),
): ChatMessage => ({
  id: response,
  role: "assistant",
  name: "Pipeline",
  content: "existing answer",
  createdAt: "2026-10-02T12:00:00Z",
  threadId: "root",
  executionGeneration: generation,
  staticPause: binding,
});
describe("static occurrence controls", () => {
  it.each(["before", "after"] as const)(
    "sends %s only with original public identity",
    (kind) => {
      const pause = staticPauseBinding(
        response,
        generation,
        "root",
        proof("a", kind),
        undefined,
      )!;
      const body = staticContinuationBody(pause, 7, generation, "Continue", []);
      expect(body).toEqual({
        project_id: 7,
        conversation_uuid: generation,
        message_id: response,
        thread_id: "root",
        static_pause_id: proof().pause_id,
        user_input: "Continue",
      });
      expect(body).not.toHaveProperty("checkpoint_id");
      expect(body).not.toHaveProperty("execution_generation");
      expect(body).not.toHaveProperty("hitl_resume");
    },
  );
  it("retains untouched leaves and dynamic controls across selected batches", () => {
    const pause = staticPauseBinding(
      response,
      generation,
      "root",
      undefined,
      inventory(),
    )!;
    const dynamic = {
      interrupt_id: "dynamic-original",
      tool_call_id: "dynamic-call",
    };
    const original = {
      ...message(pause),
      hitlInterrupt: dynamic,
      hitlInterrupts: [dynamic],
      toolActions: [],
    };
    const selected = inventory().pauses[1]!.proof.pause_id;
    expect(
      staticContinuationBody(pause, 7, generation, "Proceed", [selected]),
    ).toMatchObject({
      static_decisions: [
        {
          pause_id: selected,
          child_thread_id: "child-2",
          tool_call_id: "same-call",
          action: "continue",
          value: "Proceed",
        },
      ],
    });
    const remaining = consumeStaticPause([original], pause, [selected])[0]!;
    expect(remaining.hitlInterrupt).toBe(dynamic);
    expect(remaining.hitlInterrupts).toBe(original.hitlInterrupts);
    expect(remaining.staticPause?.kind).toBe("tools");
    if (remaining.staticPause?.kind === "tools")
      expect(remaining.staticPause.inventory.pauses).toEqual([
        inventory().pauses[0],
      ]);
    expect(original.staticPause).toBe(pause);
  });
  it("refuses stale, superseded, running and read-only Test controls", () => {
    const original = message();
    expect(currentStaticPause([original])).toBeDefined();
    expect(
      currentStaticPause([
        original,
        { ...original, id: "new", staticPause: undefined },
      ]),
    ).toBeUndefined();
    expect(
      currentStaticPause([{ ...original, executionGeneration: "different" }]),
    ).toBeUndefined();
    expect(
      currentStaticPause([{ ...original, isStreaming: true }]),
    ).toBeUndefined();
    const receipt = {
      response_message_id: response,
      execution_generation: generation,
      can_control: true,
      phase: "PAUSED",
    };
    expect(currentStaticPause([original], receipt)).toBeDefined();
    expect(
      currentStaticPause([original], { ...receipt, can_control: false }),
    ).toBeUndefined();
    expect(
      currentStaticPause([original], { ...receipt, phase: "TERMINAL" }),
    ).toBeUndefined();
    expect(
      currentStaticPause([original], {
        ...receipt,
        execution_generation: "new",
      }),
    ).toBeUndefined();
  });
  it("accepts exact descendant routing and rejects changed/unknown proof fields", () => {
    const nested = {
      ...proof(),
      descendant_path: [
        {
          node_name: "child",
          thread_id: "root/child",
          checkpoint_id: "child-checkpoint",
        },
      ],
    };
    expect(
      staticPauseBinding(response, generation, "root", nested, undefined),
    ).toBeDefined();
    expect(
      staticPauseBinding(
        response,
        generation,
        "root",
        {
          ...nested,
          descendant_path: [
            { ...nested.descendant_path[0], thread_id: "other/child" },
          ],
        },
        undefined,
      ),
    ).toBeUndefined();
    expect(
      staticPauseBinding(
        response,
        generation,
        "root",
        {
          ...nested,
          descendant_path: [
            { ...nested.descendant_path[0], state: "forbidden" },
          ],
        },
        undefined,
      ),
    ).toBeUndefined();
    expect(
      staticPauseBinding(
        response,
        generation,
        "root",
        { ...proof(), state: "forbidden" },
        undefined,
      ),
    ).toBeUndefined();
    expect(
      staticPauseBinding(
        response,
        generation,
        "root",
        { ...proof(), pending_nodes: ["other"] },
        undefined,
      ),
    ).toBeUndefined();
  });
  it("fails closed on unbounded and conflicting inventory", () => {
    const tools = inventory();
    expect(
      staticPauseBinding(response, generation, "root", undefined, tools),
    ).toBeDefined();
    expect(
      staticPauseBinding(response, generation, "root", proof(), tools),
    ).toBeUndefined();
    expect(
      staticPauseBinding(response, generation, "root", undefined, {
        ...tools,
        pauses: [tools.pauses[0], tools.pauses[0]],
      }),
    ).toBeUndefined();
    expect(
      staticPauseBinding(response, generation, "root", undefined, {
        ...tools,
        pauses: tools.pauses.map((p) => ({
          ...p,
          original_batch_event_id: "same-batch",
        })),
      }),
    ).toBeUndefined();
    expect(
      staticPauseBinding(
        response,
        generation,
        "root",
        { ...proof(), checkpoint_id: "é".repeat(129) },
        undefined,
      ),
    ).toBeUndefined();
    const pause = staticPauseBinding(
      response,
      generation,
      "root",
      undefined,
      tools,
    )!;
    expect(() =>
      staticContinuationBody(pause, 7, generation, "é".repeat(4097), [
        tools.pauses[0]!.proof.pause_id,
      ]),
    ).toThrow();
    expect(() =>
      staticContinuationBody(pause, 7, generation, "Proceed", ["unknown"]),
    ).toThrow();
  });
  it("restores original persisted identity without synthesizing HITL", () => {
    const rows = convertMessagesToChatHistory(
      [
        {
          id: 1,
          uuid: response,
          reply_to_id: 2,
          content: "existing answer",
          created_at: "2026-10-02T12:00:00Z",
          meta: {
            execution_generation: generation,
            thread_id: "root",
            pipeline_static_v1: proof(),
          },
        },
      ],
      [],
    );
    expect(rows[0]?.staticPause?.messageId).toBe(response);
    expect(rows[0]?.executionGeneration).toBe(generation);
    expect(rows[0]?.hitlInterrupt).toBeUndefined();
    expect(rows[0]?.content).toBe("existing answer");
  });
  it("settles before/after frames and clears on model replay or non-static full message", () => {
    const live = applyChatStreamFrame(
      [
        {
          ...message(undefined),
          staticPause: undefined,
          isStreaming: true,
          isRegenerating: true,
        },
      ],
      {
        type: "full_message",
        message_id: response,
        execution_generation: generation,
        response_metadata: {
          thread_id: "root",
          application_details: { agent_type: "pipeline" },
          pipeline_static_v1: proof(),
        },
      },
    );
    expect(live[0]?.staticPause).toBeDefined();
    expect(live[0]?.isStreaming).toBe(false);
    expect(live[0]?.isRegenerating).toBe(false);
    expect(live[0]?.hitlInterrupt).toBeUndefined();
    expect(
      applyChatStreamFrame(live, {
        type: "agent_llm_start",
        message_id: response,
        execution_generation: generation,
      })[0]?.staticPause,
    ).toBeUndefined();
    expect(
      applyChatStreamFrame(live, {
        type: "full_message",
        message_id: response,
        execution_generation: generation,
        response_metadata: { thread_id: "root" },
      })[0]?.staticPause,
    ).toBeUndefined();
    const changed = applyChatStreamFrame(live, {
      type: "agent_start",
      message_id: response,
      execution_generation: "ff92ccbd-3312-4c72-b20b-fddf224e7c0e",
    });
    expect(changed[0]?.staticPause).toBeUndefined();
    expect(changed[0]?.executionGeneration).not.toBe(generation);
    expect(
      applyChatStreamFrame(changed, {
        type: "full_message",
        message_id: response,
        execution_generation: generation,
        response_metadata: {
          thread_id: "root",
          application_details: { agent_type: "pipeline" },
          pipeline_static_v1: proof(),
        },
      })[0]?.staticPause,
    ).toBeUndefined();
  });
  it("retains root static inventory during descendant model activity", () => {
    const pause = staticPauseBinding(
      response,
      generation,
      "root",
      undefined,
      inventory(),
    )!;
    const before = [message(pause)];
    const after = applyChatStreamFrame(before, {
      type: "agent_llm_start",
      message_id: response,
      execution_generation: generation,
      response_metadata: { metadata: { parent_agent_name: "child-agent" } },
    });
    expect(after[0]?.staticPause).toBe(pause);
  });
  it("projects Pipeline-root inventory with only original leaf selectors", () => {
    const live = applyChatStreamFrame([message(undefined)], {
      type: "full_message",
      message_id: response,
      execution_generation: generation,
      response_metadata: {
        thread_id: "root",
        application_details: {
          agent_type: "pipeline",
          version_details: { agent_type: "pipeline" },
        },
        pipeline_static_tools_v1: inventory(),
      },
    });
    const pause = currentStaticPause(live, {
      response_message_id: response,
      execution_generation: generation,
      can_control: true,
      phase: "PAUSED",
    });
    expect(pause?.kind).toBe("tools");
    expect(pause?.messageId).toBe(response);
    expect(pause?.generation).toBe(generation);
    expect(
      staticContinuationBody(pause!, 7, generation, "Continue", [
        proof("d", "after").pause_id,
      ]),
    ).toEqual({
      project_id: 7,
      conversation_uuid: generation,
      message_id: response,
      thread_id: "root",
      static_decisions: [
        {
          pause_id: proof("d", "after").pause_id,
          child_thread_id: "child-2",
          tool_call_id: "same-call",
          action: "continue",
          value: "Continue",
        },
      ],
    });
    expect(
      currentStaticPause(live, {
        response_message_id: generation,
        execution_generation: generation,
        can_control: true,
        phase: "PAUSED",
      }),
    ).toBeUndefined();
    expect(
      currentStaticPause(live, {
        response_message_id: response,
        execution_generation: response,
        can_control: true,
        phase: "PAUSED",
      }),
    ).toBeUndefined();
  });
  it.each([
    { agent_type: "pipeline", version_details: { agent_type: "agent" } },
    { agent_type: "agent", version_details: { agent_type: "pipeline" } },
    { agent_type: "pipeline", version_details: { agent_type: "unknown" } },
    { agent_type: "unknown", version_details: { agent_type: "pipeline" } },
    {},
  ])(
    "refuses contradictory or unsupported live root kinds %j",
    (application_details) => {
      expect(
        applyChatStreamFrame([message(undefined)], {
          type: "full_message",
          message_id: response,
          execution_generation: generation,
          response_metadata: {
            thread_id: "root",
            application_details,
            pipeline_static_tools_v1: inventory(),
          },
        })[0]?.staticPause,
      ).toBeUndefined();
    },
  );
  it.each(["agent", "pipeline"])(
    "keeps version-only %s metadata compatible",
    (kind) => {
      expect(
        applyChatStreamFrame([message(undefined)], {
          type: "full_message",
          message_id: response,
          execution_generation: generation,
          response_metadata: {
            thread_id: "root",
            application_details: { version_details: { agent_type: kind } },
            pipeline_static_tools_v1: inventory(),
          },
        })[0]?.staticPause?.kind,
      ).toBe("tools");
    },
  );
  it("restores original Pipeline-root inventory from durable history without root-kind metadata", () => {
    const rows = convertMessagesToChatHistory(
      [
        {
          id: 1,
          uuid: response,
          reply_to_id: 2,
          content: "existing answer",
          created_at: "2026-10-02T12:00:00Z",
          meta: {
            execution_generation: generation,
            thread_id: "root",
            pipeline_static_tools_v1: inventory(),
          },
        },
      ],
      [],
    );
    const pause = currentStaticPause(rows, {
      response_message_id: response,
      execution_generation: generation,
      can_control: true,
      phase: "PAUSED",
    });
    expect(pause?.kind).toBe("tools");
    if (pause?.kind === "tools") expect(pause.inventory).toEqual(inventory());
    expect(rows[0]?.hitlInterrupt).toBeUndefined();
    expect(rows[0]?.content).toBe("existing answer");
  });
  it("refuses forged leaf identities and child-frame inventory", () => {
    const original = inventory();
    const forged = {
      ...original,
      pauses: original.pauses.map((pause, index) =>
        index === 0
          ? {
              ...pause,
              proof: {
                ...pause.proof,
                descendant_path: [
                  {
                    node_name: "nested",
                    thread_id: "foreign/nested",
                    checkpoint_id: "original-child-checkpoint",
                  },
                ],
              },
            }
          : pause,
      ),
    };
    expect(
      staticPauseBinding(response, generation, "root", undefined, forged),
    ).toBeUndefined();
    expect(
      applyChatStreamFrame([{ ...message(), staticPause: undefined }], {
        type: "full_message",
        message_id: response,
        execution_generation: generation,
        response_metadata: {
          thread_id: "root",
          application_details: { agent_type: "pipeline" },
          parent_agent_name: "nested-agent",
          pipeline_static_tools_v1: inventory(),
        },
      })[0]?.staticPause,
    ).toBeUndefined();
  });
});
