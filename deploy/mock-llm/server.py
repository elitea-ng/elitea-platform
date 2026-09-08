"""Deterministic OpenAI-compatible mock upstream for the standalone stack.

Why vendored rather than a pulled image: the acceptance test has to be
byte-predictable and offline, and the stack already builds two images from
source. Standard library only — no pip install, no network at build or run time.

What it serves, which is exactly what bifrost's vLLM provider asks for:

  POST /v1/chat/completions   streaming (SSE) and unary
  POST /v1/embeddings         deterministic vectors for the index plane (#93)
  GET  /v1/models             so a model list through the gateway is not empty
  GET  /healthz               for the compose healthcheck

It also serves a tiny TOOL API, so an `openapi` TOOLKIT can point at this same
process and a test can prove SERVER-SIDE that an agent actually invoked it:

  GET  /tool/openapi.json     the OpenAPI 3.0.3 document for the two routes below
  GET  /tool/status           read-only; returns a fixed document
  POST /tool/items            effectful; returns a fixed creation receipt

It keeps TWO REQUEST JOURNALS, and serves both (issue #470 for the first):

  GET    /__journal           the recorded MODEL requests, newest last
  DELETE /__journal           empty it
  GET    /tool/__journal      the recorded TOOL calls, newest last
  DELETE /tool/__journal      empty it

Two journals rather than one because the questions are different and the
windows are different: "which model did the gateway ask" is answered by the
first, "did the agent run this tool" only by the second, and a spec bounding
one window must not erase the other's evidence.

Each MODEL entry also records the SYSTEM prompt the request carried, because
that is the only observable for anything the runtime does to the instructions
(agent-variable substitution, above all): the reply echoes the last USER
message and can never show it.

The journal is the only place a test can read what the gateway actually put on
the wire. A vector width does not identify a model, and a 200 does not prove
which project's credential resolved. The journal records both: the model name
as it arrived, and a label for the credential the gateway sent. Seed a
different mock key per project and that label names the project the gateway
resolved.

The journal holds no secret. A credential that starts with the literal prefix
`mock-key-` is recorded as it is, because the seed script writes that prefix
and the value is not a secret. Any other credential is recorded as a SHA-256
prefix, so a real key can never reach the journal even if an operator points a
real credential at this mock.

The reply is an echo of the last user message, prefixed, so a test can assert
on content it supplied rather than on a fixed string that could also come from
a cached or misrouted response.

PER-REQUEST MODES, SELECTED BY THE PROMPT (see `_script_for`):

  [[mock:ask_user]]   answer with a CALL to the runtime's `ask_user` internal
                      tool instead of with text, then — once the tool result
                      comes back — with a normal answer quoting it.
  [[mock:slow]]       stream a long, scripted reply one word at a time with a
                      per-chunk delay, so a test can act while the turn is
                      still open (press Stop, navigate away, drop the stream).
  [[mock:call_tool <operationId>]]
                      answer with a CALL to that operation of an attached
                      toolkit, then — once the tool result comes back — with a
                      normal answer quoting it verbatim and ending in
                      CALL_TOOL_SENTINEL.
  [[mock:call_tool <toolName> {"json": "arguments"}]]
                      the same, with ARGUMENTS. A tool whose schema declares a
                      required field cannot be called with the empty object the
                      short form sends: a nested Elitea agent is exposed to the
                      model as a tool that requires a non-empty `task`, and the
                      runtime refuses a call without one before any child runs.
                      So the delegation journeys name the tool AND the task,
                      and the child then answers the task as an ordinary
                      request of its own — which is what makes a sub-agent hop
                      observable without a model that decides to delegate.

The marker travels in the PROMPT and nowhere else, and that is the whole point.
An environment variable or a control endpoint would be process-wide: one mock
serves every spec in the `chat-stream` project, so a mode armed by one spec
would also be armed for whatever other spec happened to be streaming at that
moment — coupling specs that share nothing else and making a failure depend on
the order they ran in. A marker in the prompt is scoped to the ONE request that
carries it, needs no setup and no teardown, and leaves the default (echo)
behaviour of every other request untouched.

It is reached as a vLLM-class credential, not an OpenAI one, and that is not
cosmetic: internal/account/account.go:235 sets AllowPrivateNetwork only for
schemas.VLLM and schemas.Ollama, and only when GATEWAY_EGRESS_ALLOWLIST is
non-empty. An `open_ai` credential pointing at this service would be refused by
bifrost's SSRF-safe dialer no matter what the allowlist says — the guard that
stops a tenant steering the gateway into the cluster.
"""

from __future__ import annotations

import base64
import hashlib
import json
import math
import os
import struct
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import NamedTuple

PORT = int(os.environ.get("MOCK_LLM_PORT", "8090"))
MODEL = os.environ.get("MOCK_LLM_MODEL", "E2E-MOCK-MODEL")
EMBEDDING_MODEL = os.environ.get("MOCK_LLM_EMBEDDING_MODEL", "E2E-MOCK-EMBEDDING")
# 1536 because that is the width every embedding artefact in this workspace is
# already cut to — `elitea-sdk/tests/tools/index/fixtures/rag_corpus_embeddings.sql`
# (text-embedding-ada-002) stores 1536-element vectors — so a corpus dumped by
# the SDK's own fixtures and one produced here are dimensionally interchangeable.
# Nothing in the platform PINS a width: the table the Go writer creates declares
# `embedding vector` with no dimension modifier
# (internal/infra/pgvector/index_meta_writer.go:660) and langchain_postgres
# leaves `embedding_length` unset, so pgvector accepts any consistent width and
# a mismatch would surface only as an operator error between two differently
# sized vectors in the SAME collection. Overridable so a test can prove the
# width actually travels end to end rather than being assumed.
EMBEDDING_DIMENSIONS = int(os.environ.get("MOCK_LLM_EMBEDDING_DIMENSIONS", "1536"))
MAX_EMBEDDING_DIMENSIONS = 4096
MAX_EMBEDDING_INPUTS = 2048
PREFIX = os.environ.get("MOCK_LLM_PREFIX", "MOCK:")
# Per-chunk delay, default 0 (fast). The chat streaming journey sets it so that
# "a token rendered before the turn finished" is a DETERMINISTIC observation
# rather than a race against a reply that arrives in one paint: with no delay
# every chunk lands within a few milliseconds and a browser can legitimately
# show the finished answer without ever painting a partial one, which would
# make an incremental assertion flaky rather than wrong.
CHUNK_DELAY_SECONDS = float(os.environ.get("MOCK_LLM_CHUNK_DELAY_MS", "0")) / 1000.0

# ── Per-request modes ────────────────────────────────────────────────────────
# Read the module docstring for WHY the selector is the prompt and not
# configuration. These constants are the wire contract between this file and
# `apps/elitea-web/e2e/streaming/chat.*.spec.ts`; changing one without the
# other leaves a spec waiting for a behaviour nothing produces.
SLOW_MARKER = "[[mock:slow]]"
ASK_USER_MARKER = "[[mock:ask_user]]"
# `[[mock:call_tool <operationId>]]` — answer with a CALL to one operation of
# the TOOL API this same process serves under /tool (see TOOL_* below), then —
# once the tool result comes back — with a normal answer quoting it.
#
# The operation name travels in the marker rather than being fixed, because the
# two toolkit journeys need different ones: a READ-ONLY GET, which the native
# runtime may execute, and an EFFECTFUL POST, which it pauses on. One mode with
# a parameter keeps the two specs on the same code path, so a break in the mode
# cannot pass one journey and fail the other for unrelated reasons.
CALL_TOOL_MARKER_PREFIX = "[[mock:call_tool "
CALL_TOOL_MARKER_SUFFIX = "]]"
# The arguments the scripted call carries when the marker names none.
#
# EMPTY IS THE RIGHT DEFAULT FOR A TOOLKIT OPERATION and the wrong one for a
# nested agent, which is why the marker takes an optional second field. Neither
# /tool operation declares a required parameter, and an argument their JSON
# Schema does not admit is refused by the runtime before any request is made
# (`additionalProperties: false`). A nested Elitea application is the opposite:
# it is presented to the model as a tool whose schema REQUIRES a non-empty
# `task` (services/elitea-worker-rust/src/agents/application_tools.rs, and the
# SDK's own `build_dynamic_application_schema`), so `{}` is refused as an
# invalid configuration and the whole turn dies before the child agent runs.
CALL_TOOL_DEFAULT_ARGUMENTS = "{}"
# The last word of the continuation reply, and the reason it exists: the
# stored assistant row is READABLE WHILE IT IS STILL BEING WRITTEN, and a tool
# result is long — the native runtime's blocked-call payload alone is ~700
# bytes. A journey that polls for something near the FRONT of that payload
# (the denial comment, the tool name) settles on a half-written row and then
# asserts against text that has not arrived yet; measured, that reported a
# missing `sensitive_tool_blocked` on a row that carried it a moment later.
# A sentinel at the very END is a settle signal that cannot be reached early,
# and it is the same trick SLOW_SENTINEL plays for the streaming journey.
CALL_TOOL_SENTINEL = "MOCKCALLTOOLEND"

# The slow reply's shape. The tail is a fixed, ordered word sequence so a
# truncated reply can be told from a complete one by CONTENT and not only by
# length: a reader that never sees SLOW_SENTINEL saw a stream that was cut
# short, whatever its byte count. Length alone would not discriminate — a
# shorter answer is also what a different prompt produces.
SLOW_CHUNKS = int(os.environ.get("MOCK_LLM_SLOW_CHUNKS", "80"))
SLOW_SENTINEL = "MOCKSTREAMEND"
# 250ms x 80 words ≈ 20s of open stream. Long enough that a browser test can
# observe a partial answer, click a control and read the store back before the
# turn would have finished on its own; short enough that a test which never
# cancels still terminates well inside a Playwright timeout.
SLOW_CHUNK_DELAY_SECONDS = (
    float(os.environ.get("MOCK_LLM_SLOW_CHUNK_DELAY_MS", "250")) / 1000.0
)

# The clarification the `[[mock:ask_user]]` script asks for.
#
# The shape is the one `AskUserRequest::from_arguments` admits and nothing
# wider (services/elitea-worker-rust/src/agents/internal_tools.rs): the
# arguments object may hold `questions` and no other key, and each question may
# hold only id/question/header/options/multi_select/allow_other. A stray key
# is InvalidInput, which the runtime surfaces as a failed turn rather than as a
# clarification — so this literal is deliberately minimal.
ASK_USER_TOOL_NAME = "ask_user"
ASK_USER_CALL_ID = "call_mock_ask_user_1"
ASK_USER_QUESTIONS = [
    {
        "id": "environment",
        "question": "Which environment should I target?",
        "header": "Environment",
        "options": [
            {"label": "Staging", "description": "the shared pre-production stack"},
            {"label": "Production", "description": "the live stack"},
        ],
        "multi_select": False,
        "allow_other": True,
    }
]
# ── The TOOL API (issue: toolkit-invocation coverage) ────────────────────────
#
# A second, tiny HTTP API served by this SAME process under /tool, so a real
# `openapi` TOOLKIT can point at it and a test can prove, SERVER-SIDE, that the
# agent runtime actually called the operation. The LLM journal above proves
# what the gateway put on the wire; it cannot prove that a TOOL ran, because a
# tool call happens between the worker and the tool's own host and never
# touches the model hop at all.
#
# It lives here rather than in a new service for the same reason the mock LLM
# is vendored: one more compose service is one more image to build, one more
# healthcheck to wait on and one more port to collide with, for a handler that
# is forty lines. The stack already builds this image and already publishes
# this port for the journal.
#
# TWO OPERATIONS, and the split is the whole point:
#
#   GET  /tool/status   read-only. `OpenApiOperation::is_read_only` is
#                       `matches!(method, GET | HEAD | OPTIONS)`
#                       (services/elitea-worker-rust/.../openapi/spec.rs), and
#                       the native runtime's direct-HITL replay admits an
#                       APPROVED call only when that is true.
#   POST /tool/items    effectful. The same runtime refuses an approved replay
#                       for it ("approved direct HITL replay remains closed for
#                       an effectful tool") and only ever produces the DENIED
#                       result, which is what chat.toolkit-hitl.spec.ts asserts.
#
# THE SERVERS URL IS HTTPS BY DEFAULT, and that is not cosmetic. The native
# Rust worker's OpenAPI client is `https_only()` and its base-URL parser
# refuses any other scheme (`parse_https_base`), so an `http://` server URL
# makes the whole TURN fail at toolset materialization rather than making one
# tool call fail. Overridable because the Python worker's SDK toolkit accepts
# `http://`, and a leg driven against that worker wants the reachable address.
TOOL_PATH_PREFIX = "/tool"
TOOL_BASE_URL = os.environ.get("MOCK_LLM_TOOL_BASE_URL", "https://llm-mock:8090/tool")
# The operation ids. `operationId` is what the worker names the ADK function
# after (`parse_operations`), what `selected_tools` filters on, and what the
# `[[mock:call_tool …]]` marker carries — one string, three consumers.
TOOL_STATUS_OPERATION = "mock_tool_status"
TOOL_CREATE_OPERATION = "mock_tool_create_item"
# Sentinels rather than bare "ok": a test asserting on the tool result has to
# be able to tell it from every other string in the transcript, including the
# echo of its own prompt.
TOOL_STATUS_SENTINEL = "MOCKTOOLSTATUS"
TOOL_CREATE_SENTINEL = "MOCKTOOLCREATED"
TOOL_STATUS_BODY = {"status": "ok", "sentinel": TOOL_STATUS_SENTINEL}
TOOL_CREATE_BODY = {"created": True, "sentinel": TOOL_CREATE_SENTINEL}
# Bounded like the LLM journal, and for the same reason.
MAX_TOOL_JOURNAL_ENTRIES = int(os.environ.get("MOCK_LLM_TOOL_JOURNAL_LIMIT", "500"))
# A tool request body is a scripted JSON object, never a batch of embeddings.
MAX_TOOL_BODY_BYTES = 64 << 10

# 16 MiB rather than 1: an embeddings batch is up to MAX_EMBEDDING_INPUTS
# token-id arrays, which is an order of magnitude larger than any chat body and
# would otherwise be rejected as "body too large" on the index path alone.
MAX_BODY_BYTES = 16 << 20
# The journal is bounded so a long soak run cannot grow the process without a
# limit. A test reads it after a few requests, so the oldest entries are the
# ones to drop.
MAX_JOURNAL_ENTRIES = int(os.environ.get("MOCK_LLM_JOURNAL_LIMIT", "500"))
# The prefix that marks a credential as a seeded mock value rather than a
# secret. Keep it in step with `seed-llm` in deploy/scripts/standalone-stack.sh.
MOCK_CREDENTIAL_PREFIX = "mock-key-"

_JOURNAL: list[dict] = []
_JOURNAL_LOCK = threading.Lock()
# The TOOL journal is SEPARATE from the LLM one on purpose. A test asserting
# "the agent called the tool" must not have to filter the model traffic out of
# its window, and — more importantly — `DELETE /__journal` must not silently
# erase the tool evidence a spec is about to read, nor the reverse.
_TOOL_JOURNAL: list[dict] = []
_TOOL_JOURNAL_LOCK = threading.Lock()


def _credential_label(authorization: str) -> str:
    """Name the credential the caller sent, without ever storing a secret.

    A seeded mock key is recorded as it is, because the test asserts on it and
    the value is public. Anything else becomes a digest prefix, which still
    tells two credentials apart but discloses nothing.
    """
    token = (authorization or "").strip()
    if token.lower().startswith("bearer "):
        token = token[7:].strip()
    if not token:
        return "none"
    if token.startswith(MOCK_CREDENTIAL_PREFIX):
        return token
    return "sha256:" + hashlib.sha256(token.encode()).hexdigest()[:16]


def _record(entry: dict) -> None:
    with _JOURNAL_LOCK:
        _JOURNAL.append(entry)
        if len(_JOURNAL) > MAX_JOURNAL_ENTRIES:
            del _JOURNAL[:-MAX_JOURNAL_ENTRIES]


def _record_tool(entry: dict) -> None:
    with _TOOL_JOURNAL_LOCK:
        _TOOL_JOURNAL.append(entry)
        if len(_TOOL_JOURNAL) > MAX_TOOL_JOURNAL_ENTRIES:
            del _TOOL_JOURNAL[:-MAX_TOOL_JOURNAL_ENTRIES]


def _message_text(message: dict) -> str:
    """One message's text, whether it arrived as a string or as typed parts."""
    content = message.get("content")
    if isinstance(content, str):
        return content
    # Multimodal content arrives as a list of typed parts.
    if isinstance(content, list):
        return " ".join(
            part.get("text", "")
            for part in content
            if isinstance(part, dict) and part.get("type") == "text"
        ).strip()
    return ""


def _system_text(messages: list[dict]) -> str:
    """Every instruction message this request carried, joined by newline.

    The ONLY place a test can read what the runtime assembled as the system
    prompt. It matters because agent-variable substitution rewrites the system
    prompt and NOTHING else: the reply is an echo of the last USER message, so
    a journey asserting on the answer alone cannot tell a substituted `{{var}}`
    from an unsubstituted one — it would pass either way.

    `developer` counts as well as `system`: an `o<digit>` model name makes the
    native runtime send that role instead
    (`services/elitea-worker-rust/src/transport/model_gateway.rs`,
    `instruction_role`), and a journal that missed it would report an empty
    prompt for a request that carried a full one.
    """
    return "\n".join(
        _message_text(message)
        for message in messages or []
        if message.get("role") in ("system", "developer")
    ).strip()


def _last_user_text(messages: list[dict]) -> str | None:
    """The last user turn's text, or None when the request carries no user turn."""
    for message in reversed(messages or []):
        if message.get("role") == "user":
            return _message_text(message)
    return None


def _reply_for(messages: list[dict]) -> str:
    """Echo the last user turn. Deterministic and attributable to the input."""
    text = _last_user_text(messages)
    if text is None:
        return f"{PREFIX} (no user message)"
    return f"{PREFIX} {text}".strip()


def _tool_result_text(messages: list[dict]) -> str | None:
    """The newest tool result in the transcript, or None when there is none.

    This is the whole resume detector for `[[mock:ask_user]]`. The marker sits
    in the USER message, which is replayed verbatim on the continuation, so a
    script that looked at the prompt alone would emit the same tool call again
    and park the turn forever. The tool result is the one thing the
    continuation carries that the first pass did not.

    `tool` is the OpenAI role for a function result; `function` is the
    pre-2023-11 spelling, accepted because nothing here should depend on which
    of the two the caller's client library emits.
    """
    for message in reversed(messages or []):
        if message.get("role") in ("tool", "function"):
            return _message_text(message)
    return None


def _tool_result_text_this_turn(messages: list[dict]) -> str | None:
    """The newest tool result BELONGING TO THE CURRENT TURN, or None.

    Distinct from `_tool_result_text` above, which scans the whole transcript.
    That is right for a single-turn script and WRONG for a conversation that
    takes a second scripted turn: the runtime replays the entire history, so a
    tool result left by turn one is still there when turn two's first pass
    arrives — and a resume detector that finds it answers with TEXT, quoting a
    stale result, instead of emitting the call the new prompt asked for.
    Measured: the second turn of the sensitive-tool journey never paused, and
    its reply quoted the FIRST turn's blocked payload.

    The current turn starts at the last `user` message, so only tool messages
    after it can belong to it. `_tool_result_text` is deliberately left alone —
    it is the contract the `[[mock:ask_user]]` journey already runs on.
    """
    for message in reversed(messages or []):
        role = message.get("role")
        if role == "user":
            return None
        if role in ("tool", "function"):
            return _message_text(message)
    return None


class _ChatScript(NamedTuple):
    """What one chat request is answered with, and how fast."""

    reply: str
    # None for an ordinary text answer; otherwise the OpenAI `tool_calls` list
    # to emit INSTEAD of content, with finish_reason `tool_calls`.
    tool_calls: list[dict] | None
    delay: float
    # Recorded in the journal so a test can prove WHICH branch served it. A
    # spec whose marker is silently dropped (a composer that trims it, a prompt
    # rewritten upstream) would otherwise read as "the feature did not happen".
    mode: str


def _slow_reply(user_text: str) -> str:
    """The `[[mock:slow]]` script: the echo, a numbered tail, then the sentinel."""
    tail = " ".join(f"slow-{index:03d}" for index in range(1, SLOW_CHUNKS + 1))
    return f"{PREFIX} {user_text} {tail} {SLOW_SENTINEL}".strip()


def _ask_user_tool_calls() -> list[dict]:
    """The one `ask_user` call the clarification script emits."""
    return [
        {
            "index": 0,
            "id": ASK_USER_CALL_ID,
            "type": "function",
            "function": {
                "name": ASK_USER_TOOL_NAME,
                "arguments": json.dumps({"questions": ASK_USER_QUESTIONS}),
            },
        }
    ]


def _tool_openapi_document() -> dict:
    """The OpenAPI 3.0.3 document describing the two /tool operations.

    Served so a toolkit's schema has ONE source of truth: the spec a journey
    pastes into the form and the routes this file answers are generated from
    the same constants, so an operation renamed here cannot leave a journey
    selecting a name nothing serves.

    Shaped for the native runtime's parser and nothing wider:
      - `servers[0].url` is absolute (a relative one is refused);
      - every operation carries an explicit `operationId`, because a generated
        name is derived from the method and path and would change under any
        path edit;
      - the POST's `requestBody` declares `application/json` — the parser
        refuses a body with no JSON media type — and is NOT required, so a
        scripted call with empty arguments is still a valid call.
    """
    return {
        "openapi": "3.0.3",
        "info": {"title": "Elitea mock tool API", "version": "1.0.0"},
        "servers": [{"url": TOOL_BASE_URL}],
        "paths": {
            "/status": {
                "get": {
                    "operationId": TOOL_STATUS_OPERATION,
                    "summary": "Read the mock tool status. Read-only.",
                    "responses": {
                        "200": {
                            "description": "the fixed status document",
                            "content": {"application/json": {"schema": {"type": "object"}}},
                        }
                    },
                }
            },
            "/items": {
                "post": {
                    "operationId": TOOL_CREATE_OPERATION,
                    "summary": "Create one mock item. Has a remote effect.",
                    "requestBody": {
                        "required": False,
                        "content": {"application/json": {"schema": {"type": "object"}}},
                    },
                    "responses": {
                        "201": {
                            "description": "the fixed creation receipt",
                            "content": {"application/json": {"schema": {"type": "object"}}},
                        }
                    },
                }
            },
        },
    }


def _marker_body(prompt: str, start: int) -> str | None:
    """The text between a marker's `[[…]]`, counting NESTED markers.

    A plain scan for the first `]]` is wrong for exactly one prompt shape, and
    it is the shape a two-level delegation needs: the task a parent hands its
    child is itself a marker, so the child's `]]` closes before the parent's.
    Reading the first one truncates the parent's arguments to invalid JSON,
    which falls through to the echo — a two-hop turn that quietly becomes a
    one-line answer.

    Counting `[[` and `]]` from the opening pair ends the marker at the `]]`
    that closes IT. For a marker with nothing nested inside, that is the first
    one, so every prompt written before this existed parses as it did — a
    stray `]]` in the trailing text is still past the end and still ignored.
    """
    depth = 0
    index = start
    while index < len(prompt) - 1:
        pair = prompt[index:index + 2]
        if pair == "[[":
            depth += 1
            index += 2
            continue
        if pair == "]]":
            depth -= 1
            if depth == 0:
                return prompt[start + len(CALL_TOOL_MARKER_PREFIX):index]
            index += 2
            continue
        index += 1
    return None


def _call_tool_marker(prompt: str) -> tuple[str, str] | None:
    """The (tool name, arguments) a `[[mock:call_tool …]]` prompt names, or None.

    Two forms, and the second exists because a required parameter cannot be
    expressed in the first:

        [[mock:call_tool mock_tool_status]]
        [[mock:call_tool elitea_agent_7_v_9 {"task": "say ready"}]]

    The name is the first whitespace-separated word, so every marker written
    before the arguments form existed parses exactly as it did — the operation
    ids these journeys call carry no spaces (`operationId`, and the runtime's
    own generated function names).

    Returns None for a prompt with no marker, for a marker naming nothing, and
    for arguments that are not a JSON object — so a malformed marker falls
    through to the default echo rather than emitting a call the runtime would
    refuse with an error that names neither the mock nor the marker. The
    arguments must therefore not contain the `]]` that closes the marker.
    """
    start = prompt.find(CALL_TOOL_MARKER_PREFIX)
    if start < 0:
        return None
    body = _marker_body(prompt, start)
    if body is None:
        return None
    operation, _, raw_arguments = body.strip().partition(" ")
    operation = operation.strip()
    if not operation:
        return None
    raw_arguments = raw_arguments.strip()
    if not raw_arguments:
        return operation, CALL_TOOL_DEFAULT_ARGUMENTS
    try:
        decoded = json.loads(raw_arguments)
    except ValueError:
        return None
    if not isinstance(decoded, dict):
        return None
    return operation, json.dumps(decoded)


def _call_tool_call_id(operation: str, prompt: str) -> str:
    """A call id that is stable for one prompt and distinct between prompts.

    A fixed literal would repeat inside one conversation, and the HITL journey
    takes two scripted turns in the same conversation — a decision recorded
    against a call id that also names an earlier call cannot be told from a
    stale one. Deriving it from the prompt keeps a rerun reproducible while
    keeping two different turns apart.
    """
    digest = hashlib.sha256(f"{operation}\0{prompt}".encode()).hexdigest()[:16]
    return f"call_mock_tool_{digest}"


def _call_tool_calls(operation: str, arguments: str, prompt: str) -> list[dict]:
    """The one scripted call to `operation`, with the arguments the marker gave.

    Empty by default because neither /tool operation declares a required
    parameter, and an argument the tool's JSON Schema does not admit is
    refused by the runtime before any request is made
    (`additionalProperties: false` in `operation_schema`). A marker that names
    arguments carries them verbatim — see CALL_TOOL_DEFAULT_ARGUMENTS for the
    tool class that cannot be called without them.
    """
    return [
        {
            "index": 0,
            "id": _call_tool_call_id(operation, prompt),
            "type": "function",
            "function": {"name": operation, "arguments": arguments},
        }
    ]


def _offered_tool_names(request: dict) -> list[str]:
    """The function names this request OFFERED the model, in order.

    Recorded in the journal because it is the only server-side, model-
    independent evidence that a toolkit materialized: a turn whose toolkit was
    dropped at assembly still answers, and still looks exactly like a turn that
    carried it. An absent or malformed `tools` array records an empty list
    rather than failing the request — the journal must never be the reason a
    turn breaks.
    """
    names = []
    for tool in request.get("tools") or []:
        if not isinstance(tool, dict):
            continue
        function = tool.get("function")
        if isinstance(function, dict) and isinstance(function.get("name"), str):
            names.append(function["name"])
    return names


def _script_for(messages: list[dict]) -> _ChatScript:
    """Choose this request's behaviour from the prompt it carries.

    Default first and unchanged: with no marker the answer is the echo at the
    configured chunk delay, which is what every other consumer of this mock
    already depends on.
    """
    user_text = _last_user_text(messages)
    prompt = user_text or ""

    if ASK_USER_MARKER in prompt:
        answered = _tool_result_text(messages)
        if answered is None:
            # First pass: park the turn on a clarification.
            return _ChatScript("", _ask_user_tool_calls(), CHUNK_DELAY_SECONDS, "ask_user")
        # Resume: quote the answer the user gave, so a test can prove the
        # substituted tool result actually reached the model rather than
        # asserting only that a second answer appeared.
        return _ChatScript(
            f"{PREFIX} resumed {answered}".strip(),
            None,
            CHUNK_DELAY_SECONDS,
            "ask_user_resumed",
        )

    marker = _call_tool_marker(prompt)
    if marker is not None:
        operation, arguments = marker
        answered = _tool_result_text_this_turn(messages)
        if answered is None:
            # First pass: invoke the tool the marker names.
            return _ChatScript(
                "",
                _call_tool_calls(operation, arguments, prompt),
                CHUNK_DELAY_SECONDS,
                "call_tool",
            )
        # Resume: quote the tool result verbatim. That is what makes the
        # answer discriminating — a run whose tool was never dispatched, or
        # whose call was BLOCKED, carries a different result string, and the
        # stored reply says which one happened without reading a single log.
        return _ChatScript(
            f"{PREFIX} tool {operation} said {answered} {CALL_TOOL_SENTINEL}".strip(),
            None,
            CHUNK_DELAY_SECONDS,
            "call_tool_resumed",
        )

    if SLOW_MARKER in prompt:
        return _ChatScript(_slow_reply(prompt), None, SLOW_CHUNK_DELAY_SECONDS, "slow")

    return _ChatScript(_reply_for(messages), None, CHUNK_DELAY_SECONDS, "echo")


def _embedding_key(item: object) -> str:
    """Canonicalize one `input` element to the string the vector is derived from.

    The OpenAI embeddings input is a union, and the index path exercises more
    than one arm of it: langchain_openai's default `check_embedding_ctx_length`
    tokenizes with tiktoken and posts LISTS OF TOKEN IDS rather than text, so a
    server that only understands strings answers the chat path and fails the
    index path. Both arms canonicalize here, which also keeps the vector for a
    given input stable no matter which arm delivered it.
    """
    if isinstance(item, str):
        return item
    if isinstance(item, list):
        return ",".join(_embedding_key(part) for part in item)
    return json.dumps(item, sort_keys=True, separators=(",", ":"))


def _split_embedding_inputs(raw_input: object) -> list:
    """Split the OpenAI `input` union into the list of items to embed.

    A bare string, or a bare token-id list, is ONE input. A list of strings, or
    a list of token-id lists, is a batch. That is how the OpenAI API
    disambiguates the same union, and the journal counts items the same way the
    handler does.
    """
    if isinstance(raw_input, list) and raw_input and isinstance(raw_input[0], (str, list)):
        return list(raw_input)
    return [raw_input]


def _input_count(raw_input: object) -> int:
    """How many items an embeddings request asks for. 0 for any other route."""
    if raw_input is None or raw_input == [] or raw_input == "":
        return 0
    return len(_split_embedding_inputs(raw_input))


def _embedding_for(text: str, dimensions: int) -> list[float]:
    """A deterministic UNIT vector of `dimensions` floats derived from `text`.

    Deterministic rather than random so a run is reproducible and a test can
    assert the exact stored vector; unit-length because langchain_openai
    averages the per-chunk vectors of a long document and divides by the norm,
    which is a ZeroDivisionError/NaN for an all-zero vector — so the zero vector
    is the one output this must never produce.

    The bytes come from SHA-256 over `text` with a counter, which gives an
    output that is fixed for a given (text, dimensions) pair, differs between
    different texts, and has no chance of being uniformly zero.
    """
    raw = text.encode("utf-8", "replace")
    values: list[float] = []
    counter = 0
    while len(values) < dimensions:
        digest = hashlib.sha256(raw + b"|" + str(counter).encode()).digest()
        # Two bytes per component, mapped into [-1, 1); 16 components per digest.
        for offset in range(0, len(digest), 2):
            if len(values) >= dimensions:
                break
            word = int.from_bytes(digest[offset:offset + 2], "big")
            values.append((word / 32768.0) - 1.0)
        counter += 1
    norm = math.sqrt(sum(value * value for value in values))
    if norm == 0.0:
        # Unreachable for SHA-256 output, but a zero vector must never escape.
        values[0] = 1.0
        norm = 1.0
    return [value / norm for value in values]


def _encode_embedding(values: list[float], encoding_format: str) -> object:
    """`float` -> a JSON array; `base64` -> little-endian float32, as OpenAI does.

    base64 is not an exotic branch to skip: the `openai` python client injects
    `encoding_format="base64"` itself when the caller did not ask for a format
    and numpy is importable, and decodes it back transparently. A mock that
    only ever returns JSON arrays therefore breaks under the very client the
    SDK uses (`elitea_sdk/runtime/clients/client.py:374`'s `OpenAIEmbeddings`).
    """
    if encoding_format == "base64":
        return base64.b64encode(struct.pack(f"<{len(values)}f", *values)).decode("ascii")
    return values


def _usage_for(reply: str) -> dict:
    """Deterministic, non-zero token counts. The gateway bills against these."""
    completion = max(1, len(reply.split()))
    return {
        "prompt_tokens": 1,
        "completion_tokens": completion,
        "total_tokens": 1 + completion,
    }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "elitea-mock-llm/1"

    # The default logger writes one line per request to stderr, which makes the
    # compose logs unreadable during a streamed run. Errors still surface via
    # log_error, which does not go through this.
    def log_message(self, fmt: str, *args: object) -> None:
        return

    def _send(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_DELETE(self) -> None:  # noqa: N802 - stdlib naming
        """Empty a journal, so a test can bound the window it asserts over."""
        path = self.path.split("?", 1)[0]
        if path == f"{TOOL_PATH_PREFIX}/__journal":
            with _TOOL_JOURNAL_LOCK:
                _TOOL_JOURNAL.clear()
            self._send(200, {"object": "list", "data": [], "count": 0})
            return
        if path != "/__journal":
            self._send(404, {"error": {"message": "not found", "type": "invalid_request_error"}})
            return
        with _JOURNAL_LOCK:
            _JOURNAL.clear()
        self._send(200, {"object": "list", "data": [], "count": 0})

    def do_GET(self) -> None:  # noqa: N802 - stdlib naming
        path = self.path.split("?", 1)[0]
        if path == "/healthz":
            self._send(200, {"status": "ok"})
            return
        if path == "/__journal":
            with _JOURNAL_LOCK:
                entries = list(_JOURNAL)
            self._send(200, {"object": "list", "data": entries, "count": len(entries)})
            return
        if path == f"{TOOL_PATH_PREFIX}/__journal":
            with _TOOL_JOURNAL_LOCK:
                entries = list(_TOOL_JOURNAL)
            self._send(200, {"object": "list", "data": entries, "count": len(entries)})
            return
        if path == f"{TOOL_PATH_PREFIX}/openapi.json":
            # NOT journaled: reading the document is not calling the tool, and
            # a journey that fetched the spec would otherwise leave an entry
            # indistinguishable from an invocation.
            self._send(200, _tool_openapi_document())
            return
        if path == f"{TOOL_PATH_PREFIX}/status":
            _record_tool({
                "method": "GET",
                "path": path,
                "operation": TOOL_STATUS_OPERATION,
                "at": time.time(),
            })
            self._send(200, TOOL_STATUS_BODY)
            return
        if path == "/v1/models":
            self._send(200, {
                "object": "list",
                "data": [
                    {"id": MODEL, "object": "model", "owned_by": "elitea-mock"},
                    {"id": EMBEDDING_MODEL, "object": "model", "owned_by": "elitea-mock"},
                ],
            })
            return
        self._send(404, {"error": {"message": "not found", "type": "invalid_request_error"}})

    def do_POST(self) -> None:  # noqa: N802 - stdlib naming
        path = self.path.split("?", 1)[0]
        if path == f"{TOOL_PATH_PREFIX}/items":
            self._tool_create_item(path)
            return
        if path not in ("/v1/chat/completions", "/v1/completions", "/v1/embeddings"):
            self._send(404, {"error": {"message": "not found", "type": "invalid_request_error"}})
            return

        length = int(self.headers.get("Content-Length") or 0)
        if length < 0 or length > MAX_BODY_BYTES:
            self._send(413, {"error": {"message": "body too large", "type": "invalid_request_error"}})
            return
        try:
            request = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            self._send(400, {"error": {"message": "invalid JSON", "type": "invalid_request_error"}})
            return

        # Recorded BEFORE the request is served, and recorded for every POST,
        # so a request that this server then rejects is still visible. A journal
        # that only held successes could not prove the negative direction: that
        # a refused model made NO upstream call at all.
        #
        # `model` is the name exactly as it arrived. Do not strip the provider
        # prefix here — the point of the record is to show what the gateway put
        # on the wire, and the prefix is part of that.
        raw_model = request.get("model")
        # The mode is resolved BEFORE the journal entry so the record names the
        # branch that served the request, not merely that a request arrived.
        script = (
            None
            if path == "/v1/embeddings"
            else _script_for(request.get("messages") or [])
        )
        _record({
            "path": path,
            "model": raw_model if isinstance(raw_model, str) else None,
            "credential": _credential_label(self.headers.get("Authorization") or ""),
            "inputs": _input_count(request.get("input")),
            "encoding_format": request.get("encoding_format"),
            "dimensions": request.get("dimensions"),
            "mode": script.mode if script else None,
            # The function names this request offered the model. Empty for
            # every request that carries no toolkit, which is every request
            # every other consumer of this journal makes.
            "tools": _offered_tool_names(request),
            # The SYSTEM prompt this request carried, verbatim. Empty for
            # /v1/embeddings, which has no messages. See `_system_text`.
            "instructions": _system_text(request.get("messages") or []),
            "at": time.time(),
        })

        if path == "/v1/embeddings":
            self._embeddings(request)
            return

        # The model echoed back is whatever was asked for, minus any
        # `provider/` prefix bifrost may not have stripped, so a client
        # comparing request and response model names is not surprised.
        model = str(request.get("model") or MODEL).split("/")[-1]
        created = int(time.time())
        completion_id = "chatcmpl-mock"

        # `script` is set for every path that reaches here: only /v1/embeddings
        # leaves it None, and that path returned above.
        if request.get("stream"):
            self._stream(completion_id, created, model, script)
        else:
            self._unary(completion_id, created, model, script)

    def _tool_create_item(self, path: str) -> None:
        """`POST /tool/items` — the EFFECTFUL operation, journaled before it answers.

        Journaled unconditionally, exactly as the LLM path is, because the
        assertion this route exists for is a NEGATIVE one: a rejected
        sensitive-tool call must leave NO entry here. An entry written only on
        success could not tell "never called" from "called and refused".
        """
        length = int(self.headers.get("Content-Length") or 0)
        if length < 0 or length > MAX_TOOL_BODY_BYTES:
            self._send(413, {"error": {"message": "body too large", "type": "invalid_request_error"}})
            return
        raw = self.rfile.read(length) if length else b""
        _record_tool({
            "method": "POST",
            "path": path,
            "operation": TOOL_CREATE_OPERATION,
            # Bounded and decoded leniently: the point of recording it is to
            # show a call happened, and a body this server cannot decode must
            # not become a 500 that reads as a tool outage.
            "body": raw.decode("utf-8", "replace")[:1024],
            "at": time.time(),
        })
        self._send(201, TOOL_CREATE_BODY)

    def _embeddings(self, request: dict) -> None:
        """`POST /v1/embeddings` — what the index plane's embedding hop calls.

        Without this route the toolkit's `index_data` run reaches the gateway,
        the gateway reaches this upstream, and the upstream answers 404 — which
        surfaces to the browser only as a generic
        `index.ingest.completed {"status":"error"}`, i.e. a whole-stack failure
        whose real cause is a missing four-line handler.
        """
        raw_input = request.get("input")
        if raw_input is None or raw_input == [] or raw_input == "":
            self._send(400, {"error": {"message": "input is required", "type": "invalid_request_error"}})
            return
        # A bare string, or a bare token-id list, is ONE input; a list of
        # strings or a list of token-id lists is a batch. The discriminator is
        # whether the first element is itself a str/list, which is exactly how
        # the OpenAI API disambiguates the same union.
        items = _split_embedding_inputs(raw_input)
        if len(items) > MAX_EMBEDDING_INPUTS:
            self._send(400, {"error": {"message": "too many inputs", "type": "invalid_request_error"}})
            return

        # `dimensions` is honoured when the caller asks (text-embedding-3
        # semantics) so a test can pin a width; otherwise the configured one.
        requested = request.get("dimensions")
        dimensions = EMBEDDING_DIMENSIONS
        if isinstance(requested, int) and not isinstance(requested, bool):
            if requested < 1 or requested > MAX_EMBEDDING_DIMENSIONS:
                self._send(400, {"error": {"message": "invalid dimensions", "type": "invalid_request_error"}})
                return
            dimensions = requested

        encoding_format = request.get("encoding_format") or "float"
        if encoding_format not in ("float", "base64"):
            self._send(400, {"error": {"message": "invalid encoding_format", "type": "invalid_request_error"}})
            return

        model = str(request.get("model") or EMBEDDING_MODEL).split("/")[-1]
        data = []
        prompt_tokens = 0
        for index, item in enumerate(items):
            key = _embedding_key(item)
            prompt_tokens += max(1, len(key.split()))
            data.append({
                "object": "embedding",
                "index": index,
                "embedding": _encode_embedding(_embedding_for(key, dimensions), encoding_format),
            })
        self._send(200, {
            "object": "list",
            "model": model,
            "data": data,
            # Non-zero for the same reason the completion path's are: the
            # gateway bills against them.
            "usage": {"prompt_tokens": prompt_tokens, "total_tokens": prompt_tokens},
        })

    def _unary(self, completion_id: str, created: int, model: str, script: _ChatScript) -> None:
        reply = script.reply
        # A tool call is an answer with NO content and finish_reason
        # `tool_calls`. Sending both would be a shape no provider produces, and
        # a client that reads content first would never dispatch the call.
        message: dict = {"role": "assistant", "content": reply or None}
        finish_reason = "stop"
        if script.tool_calls is not None:
            message = {"role": "assistant", "content": None, "tool_calls": script.tool_calls}
            finish_reason = "tool_calls"
        self._send(200, {
            "id": completion_id,
            "object": "chat.completion",
            "created": created,
            "model": model,
            "choices": [{
                "index": 0,
                "message": message,
                "finish_reason": finish_reason,
            }],
            # Non-zero and deterministic: the gateway bills against these, so
            # zeros would make the billing path untestable.
            "usage": _usage_for(reply),
        })

    def _stream(self, completion_id: str, created: int, model: str, script: _ChatScript) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        # Chunked rather than Content-Length: the body length is not known up
        # front, and HTTP/1.1 keep-alive without either framing hangs the client.
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        def event(payload: dict) -> None:
            chunk = f"data: {json.dumps(payload)}\n\n".encode()
            self.wfile.write(f"{len(chunk):X}\r\n".encode() + chunk + b"\r\n")
            self.wfile.flush()

        base = {
            "id": completion_id,
            "object": "chat.completion.chunk",
            "created": created,
            "model": model,
        }
        # A CANCELLED run closes this socket from the other end while the loop
        # below is still writing. Without this the thread dies on a
        # BrokenPipeError traceback in the compose log, which reads as a mock
        # fault in exactly the test that MEANT to cut the stream off. The
        # cancellation is the expected outcome here, so it is swallowed;
        # nothing downstream can act on it because the reader is already gone.
        try:
            event({**base, "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}]})
            if script.tool_calls is not None:
                # The whole call in ONE delta. OpenAI may split `arguments`
                # across chunks and a client must reassemble either way, but a
                # split adds nothing to test here and a partial JSON fragment
                # would be indistinguishable from a mock bug.
                event({**base, "choices": [{
                    "index": 0,
                    "delta": {"role": "assistant", "tool_calls": script.tool_calls},
                    "finish_reason": None,
                }]})
                event({**base,
                       "choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}],
                       "usage": _usage_for(script.reply)})
            else:
                # One word per chunk: a consumer that only ever sees a single
                # chunk is not actually exercising incremental streaming.
                for word in script.reply.split(" "):
                    event({**base, "choices": [{"index": 0, "delta": {"content": word + " "}, "finish_reason": None}]})
                    if script.delay:
                        time.sleep(script.delay)
                event({**base, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                       "usage": _usage_for(script.reply)})
            done = b"data: [DONE]\n\n"
            self.wfile.write(f"{len(done):X}\r\n".encode() + done + b"\r\n")
            self.wfile.write(b"0\r\n\r\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            return


def main() -> None:
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"elitea mock LLM listening on :{PORT} (model={MODEL})", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
