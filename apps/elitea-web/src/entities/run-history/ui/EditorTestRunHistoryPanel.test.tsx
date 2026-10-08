import type { ReactElement } from "react";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpResponse, http } from "msw";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  configureGeneratedClient,
  resetGeneratedClient,
} from "@/shared/api/generated/mutator";
import type {
  ConversationSummary,
  EditorTestRun,
  MessageTraceStep,
} from "@/shared/api/generated/model";
import { renderWithTheme } from "@/shared/ui/lib/testTheme";

import { server } from "../../../test/setup";
import { RunHistoryPanel } from "./RunHistoryPanel";

const identity = {
  revision: 1 as const,
  actor_id: "1",
  project_id: "1",
  application_id: "9",
  application_version_id: "12",
};
function context(id = 42): ConversationSummary {
  return {
    id,
    name: `Saved Test ${id}`,
    created_at: "2026-10-02T15:00:00Z",
    updated_at: "2026-10-02T15:00:00Z",
    duration: 0,
    message_groups_count: 2,
    is_pinned: false,
    meta: { is_hidden: true, editor_test: identity },
  };
}
function run(index = 0): EditorTestRun {
  return {
    execution_id: `run-${index}`,
    execution_generation: `generation-${index}`,
    response_message_id: `20000000-0000-4000-8000-${String(index + 1).padStart(12, "0")}`,
    response_message_group_id: 100 + index,
    trace_available: true,
    question_id: "30000000-0000-4000-8000-000000000001",
    admitted_at: "2026-10-02T15:00:00Z",
    settled_at: "2026-10-02T15:01:04Z",
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
function detail(
  id: number,
  rows: readonly EditorTestRun[],
  offset = 0,
  hasMore = false,
): object {
  return {
    id: String(id),
    source: "editor_test",
    is_private: true,
    name: `Saved Test ${id}`,
    meta: { is_hidden: true, editor_test: identity },
    editor_test_runs: { rows, limit: 50, offset, has_more: hasMore },
  };
}
function trace(id: number, group: number, label: string): MessageTraceStep {
  return {
    id,
    message_group_id: group,
    kind: "tool_call",
    tool_name: label,
    is_error: false,
  };
}
function panel(): ReactElement {
  return (
    <RunHistoryPanel
      projectId="1"
      entityName="application"
      entityId="9"
      editorTest
      onClose={vi.fn()}
    />
  );
}
function renderPanel(ui = panel()): ReturnType<typeof renderWithTheme> {
  configureGeneratedClient({ baseUrl: "/api/v2" });
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return renderWithTheme(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}
function listing(rows = [context()], total = rows.length): void {
  server.use(
    http.get("/api/v2/elitea_core/conversations/prompt_lib/1", () =>
      HttpResponse.json({ total, rows }),
    ),
  );
}
afterEach(resetGeneratedClient);

describe("editor Test receipt History", () => {
  it("refetches returned receipts after remount within the app cache window without mutations", async () => {
    configureGeneratedClient({ baseUrl: "/api/v2" });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
    });
    const requests: Request[] = [];
    let rows = [run()];
    server.use(
      http.get(
        "/api/v2/elitea_core/conversations/prompt_lib/1",
        ({ request }) => {
          requests.push(request);
          return HttpResponse.json({ total: 1, rows: [context()] });
        },
      ),
      http.get(
        "/api/v2/elitea_core/conversation/prompt_lib/1/42",
        ({ request }) => {
          requests.push(request);
          return HttpResponse.json(detail(42, rows));
        },
      ),
      http.all("/api/v2/*", ({ request }) => {
        requests.push(request);
        return new HttpResponse(null, { status: 405 });
      }),
    );
    const view = renderWithTheme(
      <QueryClientProvider client={client}>{panel()}</QueryClientProvider>,
    );
    await screen.findByTestId("editor-test-run-history-row");
    view.rerender(<QueryClientProvider client={client} />);
    const cancelled = {
      ...run(1),
      state: "CANCELLED",
      desired_state: "STOP",
    };
    rows = [cancelled, run()];
    view.rerender(
      <QueryClientProvider client={client}>{panel()}</QueryClientProvider>,
    );
    await waitFor(() =>
      expect(screen.getAllByTestId("editor-test-run-history-row")).toHaveLength(
        2,
      ),
    );
    const latest = screen.getAllByTestId("editor-test-run-history-row")[0]!;
    expect(latest).toHaveTextContent("CANCELLED");
    expect(latest).toHaveAttribute("data-execution-id", cancelled.execution_id);
    expect(latest).toHaveAttribute(
      "data-response-id",
      cancelled.response_message_id,
    );
    expect(requests.map((request) => request.method)).toEqual([
      "GET",
      "GET",
      "GET",
      "GET",
    ]);
    const reads = requests.map((request) => new URL(request.url));
    for (const url of reads.filter((url) =>
      url.pathname.includes("/conversation/"),
    )) {
      expect(url.searchParams.get("editor_test_runs")).toBe("true");
      expect(url.searchParams.get("messages_limit")).toBe("0");
    }
    client.clear();
  });

  it("refetches saved Test contexts after remount within the app cache window", async () => {
    configureGeneratedClient({ baseUrl: "/api/v2" });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
    });
    let contexts = [context()];
    const listReads: URL[] = [];
    server.use(
      http.get(
        "/api/v2/elitea_core/conversations/prompt_lib/1",
        ({ request }) => {
          listReads.push(new URL(request.url));
          return HttpResponse.json({ total: contexts.length, rows: contexts });
        },
      ),
      http.get(
        "/api/v2/elitea_core/conversation/prompt_lib/1/:id",
        ({ params }) => {
          const id = Number(params["id"]);
          return HttpResponse.json(detail(id, [run(id)]));
        },
      ),
    );
    const view = renderWithTheme(
      <QueryClientProvider client={client}>{panel()}</QueryClientProvider>,
    );
    await screen.findByTestId("editor-test-context");
    await screen.findByTestId("editor-test-run-history-row");
    view.rerender(<QueryClientProvider client={client} />);
    contexts = [context(43), context()];
    view.rerender(
      <QueryClientProvider client={client}>{panel()}</QueryClientProvider>,
    );
    await waitFor(() =>
      expect(screen.getAllByTestId("editor-test-context")).toHaveLength(2),
    );
    expect(
      await screen.findByTestId("editor-test-run-history-row"),
    ).toHaveAttribute("data-execution-id", "run-43");
    expect(listReads).toHaveLength(2);
    for (const url of listReads) {
      expect(url.searchParams.get("source")).toBe("editor_test");
      expect(url.searchParams.get("hidden")).toBe("only");
      expect(url.searchParams.get("mine")).toBe("true");
    }
    client.clear();
  });

  it("uses admitted/settled timestamps and both original trace fences, excluding other groups", async () => {
    const user = userEvent.setup();
    const reads: URL[] = [];
    listing();
    const completed = run();
    const active = {
      ...run(1),
      settled_at: null,
      state: "RUNNING",
      phase: "RUNNING" as const,
    };
    server.use(
      http.get("/api/v2/elitea_core/conversation/prompt_lib/1/42", () =>
        HttpResponse.json(detail(42, [completed, active])),
      ),
      http.get(
        "/api/v2/elitea_core/message_traces/prompt_lib/1/42",
        ({ request }) => {
          reads.push(new URL(request.url));
          return HttpResponse.json({
            total: 2,
            rows: [
              trace(1, 100, "original-tool"),
              trace(2, 101, "other-run-tool"),
            ],
          });
        },
      ),
      http.get(
        "/api/v2/elitea_core/message_trace/prompt_lib/1/1",
        ({ request }) => {
          reads.push(new URL(request.url));
          return HttpResponse.json({
            ...trace(1, 100, "original-tool"),
            text: "original detail",
          });
        },
      ),
    );
    renderPanel();
    const rows = await screen.findAllByTestId("editor-test-run-history-row");
    expect(rows[0]).toHaveTextContent("1m 4s");
    expect(rows[1]).toHaveTextContent("—");
    expect(rows[1]).not.toHaveTextContent("0s");
    await user.click(rows[0]!);
    await user.click(await screen.findByText("original-tool"));
    await screen.findByText("original detail");
    expect(screen.queryByText("other-run-tool")).not.toBeInTheDocument();
    expect(reads).toHaveLength(2);
    for (const url of reads) {
      expect(url.searchParams.get("message_group_id")).toBe("100");
      expect(url.searchParams.get("execution_id")).toBe(completed.execution_id);
      expect(url.searchParams.get("execution_generation")).toBe(
        completed.execution_generation,
      );
      expect(url.searchParams.get("response_message_id")).toBe(
        completed.response_message_id,
      );
    }
  });

  it("fails closed for older Main rows without trace availability or original group fields", async () => {
    const user = userEvent.setup();
    let traces = 0;
    listing();
    const older = run();
    delete older.trace_available;
    delete older.response_message_group_id;
    server.use(
      http.get("/api/v2/elitea_core/conversation/prompt_lib/1/42", () =>
        HttpResponse.json(detail(42, [older])),
      ),
      http.get("/api/v2/elitea_core/message_traces/prompt_lib/1/42", () => {
        traces++;
        return HttpResponse.json({ rows: [], total: 0 });
      }),
    );
    renderPanel();
    await user.click(await screen.findByTestId("editor-test-run-history-row"));
    await screen.findByText("No recorded trace for this run");
    expect(traces).toBe(0);
  });

  it("pages 57 receipt rows without borrowing selection or trace from the previous page", async () => {
    const user = userEvent.setup();
    const offsets: string[] = [];
    listing();
    server.use(
      http.get(
        "/api/v2/elitea_core/conversation/prompt_lib/1/42",
        ({ request }) => {
          const offset = Number(
            new URL(request.url).searchParams.get("runs_offset"),
          );
          offsets.push(String(offset));
          const count = offset === 0 ? 50 : 7;
          return HttpResponse.json(
            detail(
              42,
              Array.from({ length: count }, (_, i) => run(offset + i)),
              offset,
              offset === 0,
            ),
          );
        },
      ),
      http.get("/api/v2/elitea_core/message_traces/prompt_lib/1/42", () =>
        HttpResponse.json({
          rows: [trace(1, 100, "first-page-tool")],
          total: 1,
        }),
      ),
    );
    renderPanel();
    await waitFor(() =>
      expect(screen.getAllByTestId("editor-test-run-history-row")).toHaveLength(
        50,
      ),
    );
    await user.click(screen.getAllByTestId("editor-test-run-history-row")[0]!);
    await screen.findByText("first-page-tool");
    await user.click(
      within(screen.getByTestId("editor-test-run-pages")).getByRole("button", {
        name: "Next",
      }),
    );
    await waitFor(() =>
      expect(screen.getAllByTestId("editor-test-run-history-row")).toHaveLength(
        7,
      ),
    );
    expect(screen.queryByText("first-page-tool")).not.toBeInTheDocument();
    expect(screen.getByTestId("run-history-no-selection")).toBeInTheDocument();
    expect(
      screen.getAllByTestId("editor-test-run-history-row")[0],
    ).toHaveAttribute("data-execution-id", "run-50");
    expect(offsets).toEqual(["0", "50"]);
    await user.click(
      within(screen.getByTestId("editor-test-run-pages")).getByRole("button", {
        name: "Previous",
      }),
    );
    await waitFor(() =>
      expect(screen.getAllByTestId("editor-test-run-history-row")).toHaveLength(
        50,
      ),
    );
    expect(screen.getByTestId("run-history-no-selection")).toBeInTheDocument();
  });

  it.each([
    { application_version_id: "13" },
    { project_id: "2" },
    { actor_id: "999" },
  ])(
    "rejects changed Test scope %j instead of rendering its durable runs",
    async (changed) => {
      listing();
      const wrong = {
        ...detail(42, [run()]),
        meta: { is_hidden: true, editor_test: { ...identity, ...changed } },
      };
      server.use(
        http.get("/api/v2/elitea_core/conversation/prompt_lib/1/42", () =>
          HttpResponse.json(wrong),
        ),
      );
      renderPanel();
      await screen.findByText("Run history unavailable");
      expect(
        screen.queryByTestId("editor-test-run-history-row"),
      ).not.toBeInTheDocument();
    },
  );

  it.each([false, "true", 1, undefined])(
    "requires the detail's private flag to be exactly true: %j",
    async (isPrivate) => {
      listing();
      server.use(
        http.get("/api/v2/elitea_core/conversation/prompt_lib/1/42", () =>
          HttpResponse.json({ ...detail(42, [run()]), is_private: isPrivate }),
        ),
      );
      renderPanel();
      await screen.findByText("Run history unavailable");
      expect(screen.queryByTestId("editor-test-run-history-row")).not.toBeInTheDocument();
    },
  );

  it("ignores a delayed previous-context response after another saved Test is selected", async () => {
    const user = userEvent.setup();
    listing([context(42), context(43)]);
    let release: (() => void) | undefined;
    let initialRequested = false;
    let initialReturned = false;
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.get("/api/v2/elitea_core/conversation/prompt_lib/1/42", async () => {
        initialRequested = true;
        await pending;
        initialReturned = true;
        return HttpResponse.json(detail(42, [run(0)]));
      }),
      http.get("/api/v2/elitea_core/conversation/prompt_lib/1/43", () =>
        HttpResponse.json(detail(43, [run(3)])),
      ),
    );
    renderPanel();
    await waitFor(() => expect(initialRequested).toBe(true));
    await user.click((await screen.findAllByTestId("editor-test-context"))[1]!);
    const row = await screen.findByTestId("editor-test-run-history-row");
    expect(row).toHaveAttribute("data-execution-id", "run-3");
    release?.();
    await waitFor(() => expect(initialReturned).toBe(true));
    await waitFor(() =>
      expect(screen.getByTestId("editor-test-run-history-row")).toHaveAttribute(
        "data-execution-id",
        "run-3",
      ),
    );
  });
  it("pages 51 saved Test contexts and discards the prior receipt selection", async () => {
    const user = userEvent.setup();
    const offsets: string[] = [];
    server.use(
      http.get(
        "/api/v2/elitea_core/conversations/prompt_lib/1",
        ({ request }) => {
          const offset = Number(
            new URL(request.url).searchParams.get("offset"),
          );
          offsets.push(String(offset));
          const rows =
            offset === 0
              ? Array.from({ length: 50 }, (_, i) => context(42 + i))
              : [context(92)];
          return HttpResponse.json({ total: 51, rows });
        },
      ),
      http.get(
        "/api/v2/elitea_core/conversation/prompt_lib/1/:id",
        ({ params }) => {
          const id = Number(params["id"]);
          return HttpResponse.json(detail(id, [run(id)]));
        },
      ),
    );
    renderPanel();
    await waitFor(() =>
      expect(screen.getAllByTestId("editor-test-context")).toHaveLength(50),
    );
    await user.click(
      within(screen.getByTestId("editor-test-context-pages")).getByRole(
        "button",
        { name: "Next" },
      ),
    );
    await waitFor(() =>
      expect(screen.getAllByTestId("editor-test-context")).toHaveLength(1),
    );
    expect(
      await screen.findByTestId("editor-test-run-history-row"),
    ).toHaveAttribute("data-execution-id", "run-92");
    expect(offsets).toEqual(["0", "50"]);
    expect(
      within(screen.getByTestId("editor-test-context-pages")).getByRole(
        "button",
        { name: "Next" },
      ),
    ).toBeDisabled();
  });
});
