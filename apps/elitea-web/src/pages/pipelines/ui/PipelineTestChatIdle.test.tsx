import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse, http } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  configureGeneratedClient,
  resetGeneratedClient,
} from "@/shared/api/generated/mutator";
import { SocketClientContext } from "@/shared/api/socket/client";
import { createTestSocketClient } from "@/shared/api/socket/testing";
import {
  installTestEventSource,
  type TestEventSourceRegistry,
} from "@/shared/api/sse/testing";
import { resetConfigForTests } from "@/shared/config/get-config";
import { server } from "@/test/setup";

import { renderPipelinesRoute } from "../__tests__/testRouter";
import { resetPipelineTestConversationsForTests } from "../lib/usePipelineTestConversation";
import { PipelineTestChat } from "./PipelineTestChat";

const IDENTITY = {
  projectId: "9",
  applicationId: "42",
  pipelineName: "Saved pipeline",
  versionId: "7",
  agentType: "pipeline",
};
const USER = { id: "6", name: "Test user" };
const globals = globalThis as unknown as Record<string, unknown>;
let streams: TestEventSourceRegistry;
let scrollIntoView: PropertyDescriptor | undefined;

function context(index: number): Record<string, unknown> {
  return {
    id: String(500 + index),
    uuid: `00000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
    name: "Saved pipeline",
    source: "editor_test",
    is_private: true,
    meta: {
      is_hidden: true,
      editor_test: {
        revision: 1,
        actor_id: "6",
        project_id: "9",
        application_id: "42",
        application_version_id: "7",
      },
    },
    participants: [
      { id: "900", entity_name: "user", entity_meta: { id: "6" } },
      {
        id: String(900 + index),
        entity_name: "application",
        entity_meta: { id: "42", project_id: "9" },
        entity_settings: { version_id: "7", agent_type: "pipeline" },
      },
    ],
  };
}

function installRoutes(pending?: Promise<void>, participantPending?: Promise<void>) {
  const created: Record<string, unknown>[] = [];
  const admitted: { url: URL; body: Record<string, unknown> }[] = [];
  const deleted: URL[] = [];
  const participantReads: URL[] = [];
  server.use(
    http.post(
      "*/elitea_core/conversations/prompt_lib/9",
      async ({ request }) => {
        created.push((await request.json()) as Record<string, unknown>);
        await pending;
        return HttpResponse.json(context(created.length));
      },
    ),
    http.post(
      "*/elitea_core/messages/prompt_lib/9/:uuid",
      async ({ request }) => {
        admitted.push({
          url: new URL(request.url),
          body: (await request.json()) as Record<string, unknown>,
        });
        return HttpResponse.json({
          task_id: `run-${admitted.length}`,
          response_message_id: "20000000-0000-4000-8000-000000000001",
          events_url: `/api/v2/executions/9/run-${admitted.length}/events`,
        });
      },
    ),
    http.delete("*/elitea_core/*", ({ request }) => {
      deleted.push(new URL(request.url));
      return new HttpResponse(null, { status: 204 });
    }),
    http.get("*/configurations/models/9", () =>
      HttpResponse.json({
        items: [{ name: "fixture-model", project_id: "9", default: true }],
        default_model_name: "fixture-model",
      }),
    ),
    http.get("*/configurations/tts_voices/*", () =>
      HttpResponse.json({ items: [] }),
    ),
    http.get("*/elitea_core/message_traces/prompt_lib/9/:conversationId", () =>
      HttpResponse.json({ rows: [], limit: 50, offset: 0, has_more: false }),
    ),
    http.get("*/elitea_core/application/prompt_lib/9/42", async ({ request }) => {
      participantReads.push(new URL(request.url));
      await participantPending;
      return HttpResponse.json({
        id: "42",
        name: "Saved pipeline",
        versions: [{ id: "7", name: "Saved version" }],
        version_details: { id: "7", agent_type: "pipeline" },
      });
    }),
  );
  return { created, admitted, deleted, participantReads };
}

function renderDisconnectedPane(restore?: { conversationId: string; onComplete: () => void }) {
  const socket = createTestSocketClient();
  socket.disconnect();
  renderPipelinesRoute(
    <SocketClientContext.Provider value={socket}>
      <PipelineTestChat
        settings={{}}
        restore={restore}
        disableChat={false}
        slotRef={undefined}
        identity={IDENTITY}
        user={USER}
      />
    </SocketClientContext.Provider>,
    "/pipelines/all/42",
    { projectId: "9" },
  );
  return socket;
}

beforeEach(() => {
  // jsdom has no native scrolling method. Keep the browser API substitution local.
  scrollIntoView = Object.getOwnPropertyDescriptor(
    Element.prototype,
    "scrollIntoView",
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    writable: true,
    value: vi.fn(),
  });
  globals["elitea_ui_config"] = {
    vite_server_url: "/api/v2",
    vite_base_uri: "/",
    vite_public_project_id: "public-1",
  };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: "/api/v2" });
  resetPipelineTestConversationsForTests();
  streams = installTestEventSource();
});

afterEach(() => {
  if (scrollIntoView) {
    Object.defineProperty(Element.prototype, "scrollIntoView", scrollIntoView);
  } else {
    Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  }
  streams.restore();
  delete globals["elitea_ui_config"];
  resetConfigForTests();
  resetGeneratedClient();
});

describe("idle editor Test context", () => {
  it("accepts a first draft after focus while hidden Test preparation is pending", async () => {
    const user = userEvent.setup();
    let release = () => {};
    const pending = new Promise<void>((resolve) => { release = resolve; });
    let releaseParticipant = () => {};
    const participantPending = new Promise<void>((resolve) => { releaseParticipant = resolve; });
    const routes = installRoutes(pending, participantPending);
    const socket = renderDisconnectedPane();
    const input = await screen.findByTestId("chat-message-input");
    const question = "Run the Code source fixture.\nKeep this first draft.";

    // Browser focus completes before its native text input events run.
    await user.click(input);
    await waitFor(() => expect(routes.created).toHaveLength(1));
    expect(input).toBeEnabled();
    expect(input).toHaveFocus();
    // Keep real keyboard input and newline handling; paste bulk text without per-character renderer work.
    await user.keyboard("R");
    expect(input).toHaveValue("R");
    await user.paste("un the Code source fixture.");
    await user.keyboard("{Shift>}{Enter}{/Shift}");
    await user.paste("Keep this first draft.");
    expect(input).toHaveValue(question);
    expect(screen.getByRole("button", { name: "attach files" })).toBeDisabled();
    const file = new File(["fixture"], "fixture.txt", { type: "text/plain" });
    fireEvent.paste(input, { clipboardData: { items: [{ kind: "file", getAsFile: () => file }] } });
    fireEvent.drop(input, { dataTransfer: { files: [file] } });
    expect(screen.queryByText("fixture.txt")).not.toBeInTheDocument();
    expect(input).toHaveValue(question);
    await user.keyboard("{Enter}");
    expect(input).toHaveValue(question);
    expect(routes.admitted).toHaveLength(0);
    expect(streams.getOpen()).toHaveLength(0);

    release();
    await waitFor(() => expect(routes.participantReads).toHaveLength(1));
    expect(input).toBeEnabled();
    expect(screen.getByTestId("chat-send-button")).toBeDisabled();
    expect(screen.getByRole("button", { name: "attach files" })).toBeDisabled();
    await user.keyboard(" {Backspace}{Enter}");
    expect(input).toHaveValue(question);
    expect(routes.admitted).toHaveLength(0);
    releaseParticipant();
    await waitFor(() => expect(screen.getByTestId("chat-send-button")).toBeEnabled());
    expect(screen.getByTestId("chat-message-input")).toBe(input);
    expect(input).toHaveValue(question);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(routes.admitted).toHaveLength(1));
    expect(routes.admitted[0]?.body).toMatchObject({
      project_id: 9, participant_id: 901,
      conversation_uuid: context(1)["uuid"], payload: { user_input: question },
    });
    expect(routes.created[0]?.["participants"]).toMatchObject([
      { entity_settings: { version_id: "7" } },
    ]);
    await user.keyboard("{Enter}");
    expect(routes.admitted).toHaveLength(1);
    expect(socket.getEmitted("chat_predict")).toHaveLength(0);
  });

  it("keeps a selected terminal Test run read-only without creating or dispatching work", async () => {
    const user = userEvent.setup();
    const routes = installRoutes();
    const response = "20000000-0000-4000-8000-000000000001";
    server.use(http.get("*/elitea_core/conversation/prompt_lib/9/501", () => HttpResponse.json({
      ...context(1),
      editor_test_runs: { rows: [{
        response_message_id: response,
        question_id: "10000000-0000-4000-8000-000000000001",
        execution_id: "saved-run", execution_generation: "original-generation",
        phase: "TERMINAL", state: "COMPLETED", desired_state: "COMPLETED",
        admitted_at: "2026-10-04T00:00:00Z", settled_at: "2026-10-04T00:01:00Z",
        input_reference: { bundle_id: "saved", entry_id: "request", immutable_version: "1", content_digest: "a".repeat(64) },
        can_control: false,
      }], limit: 50, offset: 0, has_more: false },
    })));
    const onComplete = vi.fn();
    const socket = renderDisconnectedPane({ conversationId: "501", onComplete });
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Existing run" })).toHaveValue(response));
    const input = screen.getByTestId("chat-message-input");
    expect(input).toBeDisabled();
    await user.click(input);
    await user.keyboard("new work{Enter}");
    expect(input).toHaveValue("");
    expect(routes.created).toHaveLength(0);
    expect(routes.admitted).toHaveLength(0);
    expect(streams.getOpen()).toHaveLength(0);
    expect(socket.getEmitted("chat_predict")).toHaveLength(0);
    expect(onComplete).toHaveBeenCalledTimes(1);
  });

  it("retains the first draft while a delayed context initializes and sends it once after validation", async () => {
    const user = userEvent.setup();
    let release = () => {};
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    const routes = installRoutes(pending);
    const socket = renderDisconnectedPane();
    const input = await screen.findByTestId("chat-message-input");
    const question = "first prompt\nkeep this draft";

    // A filled draft and its focus event can share the first interaction.
    act(() => {
      input.focus();
      fireEvent.change(input, { target: { value: question } });
    });
    await waitFor(() => expect(routes.created).toHaveLength(1));
    expect(input).toHaveValue(question);
    expect(input).toBeEnabled();
    await user.keyboard("{Enter}");
    expect(routes.admitted).toHaveLength(0);
    expect(streams.getOpen()).toHaveLength(0);

    release();
    await waitFor(() => expect(screen.getByTestId("chat-send-button")).toBeEnabled());
    expect(screen.getByTestId("chat-message-input")).toBe(input);
    expect(input).toHaveValue(question);
    await user.click(input);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(routes.admitted).toHaveLength(1));
    await waitFor(() => expect(streams.getOpen()).toHaveLength(1));
    expect(routes.admitted[0]?.body).toMatchObject({
      project_id: 9,
      conversation_uuid: context(1)["uuid"],
      participant_id: 901,
      payload: { user_input: question },
    });
    expect(routes.admitted[0]?.url.searchParams.get("execution_contract")).toBe("agent.execute.application.v1");
    expect(routes.created[0]?.["participants"]).toMatchObject([
      { entity_settings: { version_id: "7" } },
    ]);
    expect(input).toHaveValue("");
    await user.keyboard("{Enter}");
    expect(routes.created).toHaveLength(1);
    expect(routes.admitted).toHaveLength(1);
    expect(socket.getEmitted("chat_predict")).toHaveLength(0);
  });

  it("allows first interaction while disconnected but blocks send until ensure validates the context", async () => {
    const user = userEvent.setup();
    let release = () => {};
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    const routes = installRoutes(pending);
    const socket = renderDisconnectedPane();
    expect(socket.getConnectionState()).toBe("disconnected");
    expect(await screen.findByTestId("chat-message-input")).toBeEnabled();
    expect(routes.created).toHaveLength(0);
    expect(routes.admitted).toHaveLength(0);

    await user.click(screen.getByTestId("chat-message-input"));
    await waitFor(() => expect(routes.created).toHaveLength(1));
    expect(screen.getByTestId("chat-message-input")).toBeEnabled();
    await user.keyboard("run saved version{Enter}");
    expect(screen.getByTestId("chat-message-input")).toHaveValue("run saved version");
    expect(routes.admitted).toHaveLength(0);
    expect(streams.getOpen()).toHaveLength(0);
    release();
    await waitFor(() => expect(screen.getByTestId("chat-send-button")).toBeEnabled());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(routes.admitted).toHaveLength(1));
    await waitFor(() => expect(streams.getOpen()).toHaveLength(1));
    expect(routes.admitted[0]?.body).toMatchObject({
      project_id: 9,
      conversation_uuid: context(1)["uuid"],
      participant_id: 901,
    });
    expect(routes.admitted[0]?.url.searchParams.get("execution_contract")).toBe(
      "agent.execute.application.v1",
    );
    expect(routes.created).toHaveLength(1);
    expect(socket.getEmitted("chat_predict")).toHaveLength(0);
  });

  it("returns Clear to an enabled idle pane and creates a new validated scope on next use", async () => {
    const user = userEvent.setup();
    const routes = installRoutes();
    const socket = renderDisconnectedPane();
    await user.click(await screen.findByTestId("edit-pipeline-test-chat"));
    await waitFor(() =>
      expect(screen.getByTestId("editor-test-clear")).toBeEnabled(),
    );
    await waitFor(() =>
      expect(screen.getByTestId("chat-message-input")).toBeEnabled(),
    );
    await user.type(screen.getByTestId("chat-message-input"), "clear this draft");
    await user.click(screen.getByTestId("editor-test-clear"));
    await waitFor(() =>
      expect(screen.getByTestId("editor-test-clear")).toBeDisabled(),
    );
    expect(screen.getByTestId("chat-message-input")).toBeEnabled();
    expect(screen.getByTestId("chat-message-input")).toHaveValue("");
    expect(routes.created).toHaveLength(1);
    expect(routes.admitted).toHaveLength(0);
    expect(routes.deleted).toHaveLength(0);

    await user.click(screen.getByTestId("chat-message-input"));
    await waitFor(() => expect(routes.created).toHaveLength(2));
    await waitFor(() =>
      expect(screen.getByTestId("chat-message-input")).toBeEnabled(),
    );
    await user.type(
      screen.getByTestId("chat-message-input"),
      "run after Clear{Enter}",
    );
    await waitFor(() => expect(routes.admitted).toHaveLength(1));
    expect(routes.admitted[0]?.body).toMatchObject({
      conversation_uuid: context(2)["uuid"],
      participant_id: 902,
    });
    await waitFor(() => expect(streams.getOpen()).toHaveLength(1));
    expect(routes.created).toHaveLength(2);
    expect(routes.deleted).toHaveLength(0);
    expect(socket.getEmitted("chat_predict")).toHaveLength(0);
  });
});
