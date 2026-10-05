import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse, http } from "msw";
import { afterEach, describe, expect, it } from "vitest";

import type { EditorTestRun } from "@/shared/api/generated/model";
import {
  configureGeneratedClient,
  resetGeneratedClient,
} from "@/shared/api/generated/mutator";
import { renderWithTheme } from "@/shared/ui/lib/testTheme";

import { server } from "../../../test/setup";
import { RunHistoryTrace } from "./RunHistoryTrace";

function run(generation = "original"): EditorTestRun {
  return {
    execution_id: generation,
    execution_generation: generation,
    response_message_id: "20000000-0000-4000-8000-000000000001",
    response_message_group_id: 100,
    trace_available: true,
    question_id: "30000000-0000-4000-8000-000000000001",
    admitted_at: "2026-10-02T15:00:00Z",
    settled_at: "2026-10-02T15:01:00Z",
    state: "COMPLETED",
    desired_state: "RUN",
    phase: "TERMINAL",
    can_control: false,
    input_reference: {
      bundle_id: "bundle",
      entry_id: "entry",
      immutable_version: "1",
      content_digest: "a".repeat(64),
    },
  };
}
function step(id: number, name: string): object {
  return {
    id,
    message_group_id: 100,
    kind: "tool_call",
    tool_name: name,
    is_error: false,
  };
}
afterEach(resetGeneratedClient);

describe("receipt-bound trace races", () => {
  it("discards delayed detail when regeneration reuses the same response group", async () => {
    const user = userEvent.setup();
    configureGeneratedClient({ baseUrl: "/api/v2" });
    let release: (() => void) | undefined;
    let originalRequested = false;
    let originalReturned = false;
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.get(
        "/api/v2/elitea_core/message_traces/prompt_lib/1/42",
        ({ request }) => {
          const current =
            new URL(request.url).searchParams.get("execution_generation") ===
            "replacement";
          return HttpResponse.json({
            total: 1,
            rows: [
              step(
                current ? 2 : 1,
                current ? "replacement-tool" : "original-tool",
              ),
            ],
          });
        },
      ),
      http.get("/api/v2/elitea_core/message_trace/prompt_lib/1/1", async () => {
        originalRequested = true;
        await pending;
        originalReturned = true;
        return HttpResponse.json({
          ...step(1, "original-tool"),
          text: "late original detail",
        });
      }),
      http.get("/api/v2/elitea_core/message_trace/prompt_lib/1/2", () =>
        HttpResponse.json({
          ...step(2, "replacement-tool"),
          text: "replacement detail",
        }),
      ),
    );
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const renderRun = (identity: EditorTestRun) => (
      <QueryClientProvider client={client}>
        <RunHistoryTrace
          projectId="1"
          conversationId="42"
          editorTestRun={identity}
        />
      </QueryClientProvider>
    );
    const view = renderWithTheme(renderRun(run()));
    await user.click(await screen.findByText("original-tool"));
    await waitFor(() => expect(originalRequested).toBe(true));
    view.rerender(renderRun(run("replacement")));
    await screen.findByText("replacement-tool");
    release?.();
    await waitFor(() => expect(originalReturned).toBe(true));
    await user.click(screen.getByText("replacement-tool"));
    await screen.findByText("replacement detail");
    await waitFor(() =>
      expect(
        screen.queryByText("late original detail"),
      ).not.toBeInTheDocument(),
    );
  });
  it("refuses a detail payload that belongs to a different response group", async () => {
    const user = userEvent.setup();
    configureGeneratedClient({ baseUrl: "/api/v2" });
    server.use(
      http.get("/api/v2/elitea_core/message_traces/prompt_lib/1/42", () =>
        HttpResponse.json({ total: 1, rows: [step(1, "original-tool")] }),
      ),
      http.get("/api/v2/elitea_core/message_trace/prompt_lib/1/1", () =>
        HttpResponse.json({
          ...step(1, "original-tool"),
          message_group_id: 101,
          text: "foreign detail",
        }),
      ),
    );
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    renderWithTheme(
      <QueryClientProvider client={client}>
        <RunHistoryTrace
          projectId="1"
          conversationId="42"
          editorTestRun={run()}
        />
      </QueryClientProvider>,
    );
    await user.click(await screen.findByText("original-tool"));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(screen.queryByText("foreign detail")).not.toBeInTheDocument();
  });
});
