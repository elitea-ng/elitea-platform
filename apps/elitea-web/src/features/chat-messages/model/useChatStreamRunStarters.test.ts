/**
 * useChatStreamRunStarters.test.ts — the fallback decision, as a value.
 *
 * `useChatStreamTransport.test.tsx` already drives both ends of this module
 * through real HTTP (msw) and a real stream: a 400 falls back, a 422 keeps its
 * server sentence, a 200 with no `events_url` falls back. What it cannot reach
 * from there is the rest of the classification table — an `auth` failure, a
 * network failure, an abort, a 405, an error that is not an `EliteaApiError`
 * at all — because those need a transport that fails in a way msw does not
 * model. That table is the whole contract (fall back to the socket, or tell
 * the user the server refused), so it is asserted directly here.
 */
import { describe, expect, it } from "vitest";

import { EliteaApiError } from "@/shared/api/generated/mutator";

import {
  classifyStartFailure,
  nonEmptyString,
} from "./useChatStreamRunStarters";

const URL = "/api/v2/elitea_core/messages/prompt_lib/7/uuid-1";

function httpError(status: number, body?: unknown): EliteaApiError {
  return new EliteaApiError({ kind: "http", status, url: URL, body });
}

describe("classifyStartFailure", () => {
  it("reads a 404 as an absent route, which is the only safe reason to fall back", () => {
    // The route not existing is what "this backend has not landed the SSE
    // path" means. Everything else is the server ANSWERING.
    expect(classifyStartFailure(httpError(404))).toEqual({
      started: false,
      reason: "no-transport",
    });
  });

  it("reads a 405 the same way — the path exists, the method does not", () => {
    expect(classifyStartFailure(httpError(405))).toEqual({
      started: false,
      reason: "no-transport",
    });
  });

  it("keeps a refusal a refusal, so the caller does not re-run the turn over the socket", () => {
    expect(
      classifyStartFailure(
        httpError(422, {
          error: "unsupported_agent_execution",
          message: "This agent turn requires the current execution path.",
        }),
      ),
    ).toEqual({
      started: false,
      reason: "rejected",
      message: "This agent turn requires the current execution path.",
    });
  });

  it("shows a refused composition by its named text (FU-249: a Parallel in a pipeline run as an agent's tool)", () => {
    const message =
      'Pipeline "FP inner parallel" has Parallel node "inner", so it cannot run as a tool of an agent or inside a Parallel branch ("FP outer" → "FP inner parallel"). Run pipeline "FP inner parallel" from a pipeline Agent node outside any Parallel branch, start it directly, or remove the attachment before saving.';
    expect(
      classifyStartFailure(
        httpError(422, { error: "APPLICATION_COMPOSITION_INVALID", message }),
      ),
    ).toEqual({ started: false, reason: "rejected", message });
  });

  it("prefers safe_message, the field every Go producer actually writes", () => {
    expect(
      classifyStartFailure(
        httpError(400, {
          safe_message: "Configuration type is not supported.",
          message: "generic",
          error: "generic",
        }),
      ),
    ).toEqual({
      started: false,
      reason: "rejected",
      message: "Configuration type is not supported.",
    });
  });

  it("skips a blank field rather than putting whitespace on screen", () => {
    expect(
      classifyStartFailure(httpError(400, { safe_message: "   ", error: "no" })),
    ).toEqual({ started: false, reason: "rejected", message: "no" });
  });

  it("names the status when the body carries no sentence at all", () => {
    expect(classifyStartFailure(httpError(500, null))).toEqual({
      started: false,
      reason: "rejected",
      message: "The agent run could not start (HTTP 500).",
    });
  });

  it("does not fall back on an auth failure — the socket would be refused too", () => {
    expect(
      classifyStartFailure(
        new EliteaApiError({ kind: "auth", status: 401, url: URL }),
      ),
    ).toEqual({
      started: false,
      reason: "rejected",
      message: "This session is not authorized to start the agent run.",
    });
  });

  it("does not fall back when the service could not be reached", () => {
    // An unreachable API is not evidence that this backend serves no SSE
    // path; the socket sits behind the same outage.
    expect(
      classifyStartFailure(
        new EliteaApiError({
          kind: "network",
          url: URL,
          message: "failed to fetch",
          cause: undefined,
        }),
      ),
    ).toEqual({
      started: false,
      reason: "rejected",
      message: "The agent service could not be reached.",
    });
  });

  it("reports an aborted start as a refusal, not an absent transport", () => {
    expect(
      classifyStartFailure(new EliteaApiError({ kind: "aborted", url: URL })),
    ).toEqual({
      started: false,
      reason: "rejected",
      message: "The agent start request was cancelled.",
    });
  });

  it("treats a throw that is not an API failure as no transport", () => {
    // A TypeError from inside the client means the request never became an
    // answer, so the socket is still worth trying.
    expect(classifyStartFailure(new TypeError("boom"))).toEqual({
      started: false,
      reason: "no-transport",
    });
    expect(classifyStartFailure(undefined)).toEqual({
      started: false,
      reason: "no-transport",
    });
  });
});

describe("nonEmptyString", () => {
  it("keeps a real id and drops every way the wire says there is none", () => {
    expect(nonEmptyString("62e87daa")).toBe("62e87daa");
    // `""` is how a request body spells an absent question id, and `null` is
    // how the durable replay serialises one — both mean the same thing.
    expect(nonEmptyString("")).toBeUndefined();
    expect(nonEmptyString(null)).toBeUndefined();
    expect(nonEmptyString(undefined)).toBeUndefined();
    expect(nonEmptyString(7)).toBeUndefined();
  });
});
