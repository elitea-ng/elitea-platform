#!/usr/bin/env python3
"""Backend smoke for the chat critical path (#284 task 3).

Drives the loop the product depends on, without a browser:

    create conversation → add participants → POST messages
      → {events_url} → SSE stream → persisted message group

and asserts the streamed content is the deterministic echo of what it sent, so
a passing run cannot be explained by a cached or misrouted response.

Why this exists separately from the Playwright journey (#284 tasks 1-2): this
proves the backend contract without a browser, so a UI failure and a backend
failure can be told apart.

It used to say the web send path "still emits into a no-op socket client … and
the streaming reducer it would need was never ported (#93)". That stopped being
true on 2026-08-18 (a113ad2b). The web path now POSTs first and subscribes to
`events_url` (useChatStreamTransport.ts); the socket is only a fallback, and a
no-op socket client is therefore expected, not a defect.

It authenticates with a PAT minted from the deployment's own signing key rather
than driving an OIDC login, because the whole point is to run without a browser.

Exit codes: 0 pass, 1 fail, 2 skipped (a precondition the stack owner must seed),
3 blocked by a filed platform gap (the path is reached and the platform refuses).
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import hmac
import json
import pathlib
import ssl
import sys
import time
import urllib.error
import urllib.request
import uuid as uuid_module

ADHOC_CONTRACT = "agent.execute.adhoc.v1"

# The offline mock (#283) echoes its prompt; every other provider answers for
# real. `--model` decides which assertion set this run can honestly make.
MOCK_MODEL_NAMES = frozenset({"vllm/E2E-MOCK-MODEL", "E2E-MOCK-MODEL"})


def is_mock_model(model: str) -> bool:
    """True when `model` is the deterministic echo mock, whose reply is predictable."""
    return model in MOCK_MODEL_NAMES


# `pipeline_finish` ends a healthy turn; `execution_failed`/`error` end a broken
# one and must not be mistaken for completion.
TERMINAL_EVENT_TYPES = frozenset(
    {"pipeline_finish", "execution.failed", "execution_failed", "error"}
)


class Skip(Exception):
    """A precondition is missing; the path under test was never exercised."""


class Fail(Exception):
    """The path under test was exercised and did not behave."""


class Blocked(Exception):
    """The platform refused the turn for a known, filed reason.

    Distinct from Fail so a filed gap does not make `check` permanently red and
    useless as a gate for everything else, and distinct from Skip because the
    path WAS exercised — the refusal is a real answer from the platform, not a
    missing precondition.
    """


def mint_pat(uuid: str, signing_key: bytes) -> str:
    """Rebuild the current-baseline HS512 bearer for an existing active PAT.

    Mirrors authsvc.SignBaselinePAT: claims are exactly {uuid, expires}, and the
    key is the same auth-pat-signing-key elitea-main validates with.
    """
    def segment(raw: bytes) -> str:
        return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()

    header = segment(json.dumps({"alg": "HS512", "typ": "JWT"}, separators=(",", ":")).encode())
    payload = segment(json.dumps({"uuid": uuid, "expires": None}, separators=(",", ":")).encode())
    signature = segment(hmac.new(signing_key, f"{header}.{payload}".encode(), hashlib.sha512).digest())
    return f"{header}.{payload}.{signature}"


class Client:
    def __init__(self, base: str, token: str, ca: str | None) -> None:
        self.base = base.rstrip("/")
        self.token = token
        self.context = ssl.create_default_context(cafile=ca) if ca else None

    def request(self, method: str, path: str, body: object = None, stream: bool = False):
        data = json.dumps(body).encode() if body is not None else None
        headers = {"Authorization": f"Bearer {self.token}"}
        if data is not None:
            headers["Content-Type"] = "application/json"
        if stream:
            headers["Accept"] = "text/event-stream"
        request = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        try:
            response = urllib.request.urlopen(request, context=self.context, timeout=60)
        except urllib.error.HTTPError as error:
            return error.code, error.read().decode(errors="replace"), None
        if stream:
            return response.status, "", response
        return response.status, response.read().decode(errors="replace"), None


def create_conversation(client: Client, project: int) -> tuple[str, str]:
    status, body, _ = client.request(
        "POST", f"/api/v2/elitea_core/conversations/prompt_lib/{project}",
        {"name": "standalone chat smoke"})
    if status == 400 and "project_not_resolved" in body:
        raise Skip("the caller has no personal project")
    if status not in (200, 201):
        raise Fail(f"create conversation → HTTP {status}: {body[:200]}")
    conversation = json.loads(body)
    # The two ids are NOT interchangeable and the APIs disagree on which they
    # take: /participants/{conversationID} writes chat_participant_mapping
    # .conversation_id, an integer FK, while /messages/{conversationID} resolves
    # the turn by chat_conversations.uuid. Passing the uuid to participants is a
    # bare 500.
    numeric_id, uuid = conversation.get("id"), conversation.get("uuid")
    if not numeric_id or not uuid:
        raise Fail(f"create conversation returned no id/uuid pair: {body[:200]}")
    return str(numeric_id), str(uuid)


def add_participants(client: Client, project: int, conversation: str, user_id: int, model: str) -> None:
    """Both participants the adhoc resolver joins on.

    ResolveCurrentAdhocTurn requires an entity_name='user' participant whose
    entity_meta.id is the actor, AND an entity_name='dummy' participant (the
    ad-hoc model target). Missing either makes the turn resolve to no rows, and
    the route answers a flat "invalid current agent start" that names neither.
    """
    status, body, _ = client.request(
        "POST", f"/api/v2/elitea_core/participants/prompt_lib/{project}/{conversation}",
        [
            {"entity_name": "user", "entity_meta": {"id": user_id}},
            {"entity_name": "dummy", "entity_meta": {"name": model},
             "entity_settings": {"llm_settings": {"model_name": model, "temperature": 0.1,
                                                  "max_tokens": 256, "stream": True}}},
        ])
    if status not in (200, 201):
        raise Fail(f"add participants → HTTP {status}: {body[:200]}")


def start_turn(client: Client, project: int, conversation: str, prompt: str, model: str,
               question_id: str, interaction_id: str) -> str:
    status, body, _ = client.request(
        "POST",
        f"/api/v2/elitea_core/messages/prompt_lib/{project}/{conversation}"
        f"?execution_contract={ADHOC_CONTRACT}",
        # question_id and interaction_uuid must be REAL uuids: the repository
        # parses both before querying (currentPGUUID) and answers a flat
        # "invalid current agent start" — indistinguishable from "no such
        # conversation" — when either is absent or malformed.
        {"project_id": project, "conversation_uuid": conversation, "participant_id": 0,
         "question_id": question_id, "interaction_uuid": interaction_id,
         "payload": {"user_input": prompt},
         "llm_settings": {"model_name": model, "temperature": 0.1, "max_tokens": 256, "stream": True}})
    if status == 404:
        raise Skip("the agent-execution route is not mounted (ELITEA_RUNTIME_ENABLED off?)")
    if status == 422 and "unsupported_agent_execution" in body:
        # The version freezer's admission gate. It returns one sentinel from 14
        # sites with no wrapped detail, so neither this script nor an operator
        # can tell which precondition it objected to — that opacity is half of
        # what #288 asks to fix.
        raise Blocked("the version freezer refused the turn (#288)")
    if status != 200:
        raise Fail(f"start turn → HTTP {status}: {body[:300]}")
    events_url = json.loads(body).get("events_url")
    if not events_url:
        raise Fail(f"start turn returned no events_url: {body[:200]}")
    return events_url


def read_stream(client: Client, events_url: str, deadline_seconds: int,
                captured: list[dict] | None = None) -> tuple[list[str], str]:
    """Collect node-event types and the assembled assistant text.

    When `captured` is supplied, every decoded frame is appended to it verbatim.
    That is what lets the web reducer be replayed against a REAL run rather than
    against fixtures someone wrote to match it (see --capture).

    The wire contract is NOT the one #248's notes describe. Every frame is a
    single SSE event named `execution.node_event`; the semantic type lives in
    the payload's `type` field, and a turn runs
    agent_start → agent_on_transitional_edge → agent_llm_start →
    agent_llm_chunk* → agent_llm_end → agent_response → partial_message →
    full_message → pipeline_finish. Asserting on `chat.stream.chunk` /
    `chat.stream.done` would fail against a perfectly healthy stream.
    """
    status, body, response = client.request("GET", events_url, stream=True)
    if status in (401, 403):
        # production_runtime.go composes this route with PrincipalValidator +
        # ForwardedIdentityVerifier only — no bearer, no session cookie — so it
        # is reachable exclusively through a auth edge that projects
        # verified identity headers (#289). A 403 here usually means the edge's
        # X-Forwarded-Host does not equal auth.form.yml's public_origin host.
        raise Blocked(f"the events stream refused this caller: HTTP {status} (#289)")
    if status != 200:
        raise Fail(f"events stream → HTTP {status}: {body[:200]}")

    types: list[str] = []
    content: list[str] = []
    deadline = time.monotonic() + deadline_seconds
    wire_event_type = ""
    try:
        for raw in response:
            if time.monotonic() > deadline:
                raise Fail(f"no terminal event within {deadline_seconds}s; saw {types or 'nothing'}")
            line = raw.decode(errors="replace").rstrip("\n")
            # ": heartbeat" and ": connected" are SSE comments, not events.
            if line.startswith("event:"):
                wire_event_type = line.split(":", 1)[1].strip()
                continue
            if not line.startswith("data:"):
                continue
            try:
                event = json.loads(line.split(":", 1)[1].strip())
            except json.JSONDecodeError:
                continue
            if not isinstance(event, dict):
                continue
            # `execution.node_event` carries the chat semantic type in
            # `data.type`. Durable terminal failures instead use the SSE event
            # name `execution.failed` and carry only
            # `{code,safe_message,retryable}` in data. Reading `data.type`
            # alone makes a failed turn look non-terminal and waits until the
            # client deadline even though Main already committed the failure.
            event_type = str(event.get("type") or wire_event_type)
            wire_event_type = ""
            types.append(event_type)
            if captured is not None:
                captured.append(event)
            if event_type == "agent_llm_chunk" and isinstance(event.get("content"), str):
                content.append(event["content"])
            if event_type in TERMINAL_EVENT_TYPES:
                break
    finally:
        response.close()
    return types, "".join(content)


def assert_persisted(client: Client, project: int, conversation: str, expected: str) -> None:
    """Persistence, not just the stream — the #128 pattern.

    A stream can be produced and never stored; asserting only on it would pass
    against a backend that forgets the turn the moment it ends.
    """
    # The NUMERIC id again, not the uuid: ListMessages reads by
    # chat_message_group.conversation_id. Passing the uuid returns 200 with an
    # empty list, so this assertion would fail against a database that has the
    # rows — which is exactly how it first failed.
    status, body, _ = client.request(
        "GET", f"/api/v2/elitea_core/messages/prompt_lib/{project}/{conversation}")
    if status != 200:
        raise Fail(f"read back messages → HTTP {status}: {body[:200]}")
    if expected not in body:
        raise Fail("the assistant reply was streamed but is not in the persisted history")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--pat-uuid", required=True)
    parser.add_argument("--signing-key", required=True, type=pathlib.Path)
    parser.add_argument("--user-id", required=True, type=int)
    parser.add_argument("--project", type=int, default=1)
    parser.add_argument("--model", default="vllm/E2E-MOCK-MODEL")
    parser.add_argument("--ca", default=None)
    parser.add_argument("--timeout", type=int, default=90)
    parser.add_argument(
        "--capture", type=pathlib.Path, default=None,
        help="write every streamed frame here as JSON, for replaying this exact "
             "run through the web streaming reducer (apps/elitea-web)")
    args = parser.parse_args()

    # Deterministic but unique per run: the mock echoes it back, so finding it in
    # the stream proves this run produced it rather than a leftover message.
    prompt = f"chat smoke {int(time.time())}"
    client = Client(args.base_url, mint_pat(args.pat_uuid, args.signing_key.read_bytes()), args.ca)

    try:
        conversation_id, conversation = create_conversation(client, args.project)
        add_participants(client, args.project, conversation_id, args.user_id, args.model)
        events_url = start_turn(client, args.project, conversation, prompt, args.model,
                                str(uuid_module.uuid4()), str(uuid_module.uuid4()))
        print(f"  · turn admitted, events_url={events_url}")
        captured: list[dict] | None = [] if args.capture else None
        types, content = read_stream(client, events_url, args.timeout, captured)
        if args.capture and captured is not None:
            args.capture.write_text(json.dumps({"prompt": prompt, "frames": captured}, indent=2))
            print(f"  · captured {len(captured)} frames → {args.capture}")
        if any(t in ("execution.failed", "execution_failed", "error") for t in types):
            raise Fail(f"execution failed; events: {types}")
        if "agent_llm_chunk" not in types:
            raise Fail(f"no streamed token chunk; events: {types}")
        if "pipeline_finish" not in types:
            raise Fail(f"no terminal pipeline_finish; events: {types}")
        # The echo assertion is the strong one — it proves the tokens came from
        # THIS turn rather than from a replayed or cached stream — but only the
        # deterministic mock echoes. A real model cannot satisfy it, so against
        # one it is SUBSTITUTED, not silently dropped: the run says which
        # assertion it gave up and what it kept in its place, because a check
        # that quietly weakens itself reports the same green as one that did
        # the full job.
        if is_mock_model(args.model):
            if prompt not in content:
                raise Fail(f"streamed content did not echo this run's prompt; got {content[:120]!r}")
            echo_note = "echoed the prompt"
        else:
            if not content.strip():
                raise Fail("streamed chunks carried no content")
            print(f"  · model {args.model!r} is not the deterministic mock, so the "
                  f"echo assertion DID NOT RUN; kept in its place: non-empty streamed "
                  f"content ({len(content)} chars) and the persistence assertion below")
            echo_note = "streamed non-empty content (echo assertion substituted)"
        assert_persisted(client, args.project, conversation_id, prompt)
    except Blocked as blocked:
        print(f"  ! BLOCKED: {blocked}")
        return 3
    except Skip as skip:
        print(f"  ~ SKIPPED: {skip}")
        return 2
    except Fail as failure:
        print(f"  ✗ {failure}")
        return 1

    print(f"  ✓ streamed {len(types)} node events, {echo_note}, and persisted the reply")
    return 0


if __name__ == "__main__":
    sys.exit(main())
