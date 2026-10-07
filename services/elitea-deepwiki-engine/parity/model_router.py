#!/usr/bin/env python3
"""A routing, recording proxy in front of two OpenAI-compatible model servers.

The benchmark (docs/benchmark-2026-10.md) runs both DeepWiki engines against
real models, and both engines take ONE ``api_base``. This proxy is that base:

* ``POST /v1/chat/completions`` → the chat server (streaming SSE is passed
  through chunk by chunk);
* ``POST /v1/embeddings``       → the embedding server;
* ``GET  /v1/models``           → both servers' lists, merged.

Every proxied request is appended to ``--log`` as one JSON line: the current
run label, start/end (epoch seconds), latency, time to first byte, status,
and the token usage the server reported (``prompt_tokens``,
``completion_tokens``, ``reasoning_tokens``; for embeddings the input count).
The model time of a run is then the union of its request intervals, and the
engine time is the run's wall time minus that union.

A streamed chat request gets ``stream_options.include_usage`` added so the
server reports usage; the usage-only chunk that produces (``choices: []``) is
recorded and NOT forwarded, unless the client asked for usage itself, so the
client sees exactly the stream it would have seen without the proxy.

``POST /_label`` with ``{"label": "..."}`` sets the label of the requests
that follow (the harness sets one per engine x corpus x phase). With
``--capture-dir``, ``{"label": ..., "capture": true}`` also writes each
request body and the raw response (the SSE stream as received) of that
label to ``<capture-dir>/<n>.json`` — for diagnosing one case, not for runs.

    python parity/model_router.py --port 18950 \\
        --chat http://<chat-host>:8000 --embed http://<embed-host>:9000 \\
        --log ~/.cache/elitea-dw-bench/model_requests.jsonl
"""

from __future__ import annotations

import argparse
import json
import time
from pathlib import Path

from aiohttp import ClientSession, ClientTimeout, TCPConnector, web

HOP = {"host", "content-length", "transfer-encoding", "connection", "accept-encoding"}


class Router:
    def __init__(self, chat: str, embed: str, log: Path) -> None:
        self.chat = chat.rstrip("/")
        self.embed = embed.rstrip("/")
        self.log = log.open("a", encoding="utf-8")
        self.label = "unlabelled"
        self.session: ClientSession | None = None
        self.inflight = 0
        self.capture_dir: Path | None = None
        self.capture = False
        self.captured = 0

    async def start(self, _app) -> None:
        self.session = ClientSession(
            timeout=ClientTimeout(total=None, sock_read=1800),
            connector=TCPConnector(limit=0),
            auto_decompress=True,
        )

    async def stop(self, _app) -> None:
        if self.session:
            await self.session.close()
        self.log.close()

    def record(self, entry: dict) -> None:
        entry["label"] = entry.get("label", self.label)
        self.log.write(json.dumps(entry) + "\n")
        self.log.flush()

    async def set_label(self, request: web.Request) -> web.Response:
        body = await request.json()
        self.label = str(body.get("label") or "unlabelled")
        self.capture = bool(body.get("capture")) and self.capture_dir is not None
        return web.json_response({"label": self.label, "capture": self.capture})

    async def models(self, _request: web.Request) -> web.Response:
        data = []
        for base in (self.chat, self.embed):
            try:
                async with self.session.get(f"{base}/v1/models") as r:
                    data.extend((await r.json()).get("data", []))
            except Exception:  # noqa: BLE001 - a down server just lists nothing
                pass
        return web.json_response({"object": "list", "data": data})

    async def proxy(self, request: web.Request) -> web.StreamResponse:
        path = request.path
        kind = "embeddings" if path.endswith("/embeddings") else "chat"
        base = self.embed if kind == "embeddings" else self.chat
        raw = await request.read()
        label = self.label
        try:
            body = json.loads(raw) if raw else {}
        except ValueError:
            body = None
        stream = bool(isinstance(body, dict) and body.get("stream"))
        client_wants_usage = bool(
            stream and isinstance(body.get("stream_options"), dict)
            and body["stream_options"].get("include_usage")
        )
        if stream and not client_wants_usage:
            body = dict(body)
            body["stream_options"] = {**(body.get("stream_options") or {}), "include_usage": True}
            raw = json.dumps(body).encode()
        entry = {
            "kind": kind, "path": path, "label": label, "stream": stream,
            "model": body.get("model") if isinstance(body, dict) else None,
            "start": time.time(),
        }
        if kind == "embeddings" and isinstance(body, dict):
            inputs = body.get("input")
            entry["inputs"] = len(inputs) if isinstance(inputs, list) else 1
            # LangChain's OpenAIEmbeddings sends cl100k TOKEN IDS, not text;
            # record which form arrived (a non-OpenAI model reads ids as its own).
            first = inputs[0] if isinstance(inputs, list) and inputs else inputs
            entry["input_form"] = (
                "token_ids" if isinstance(first, list) or isinstance(first, int) else "text"
            )
            entry["input_chars"] = (
                sum(len(x) for x in inputs if isinstance(x, str)) if isinstance(inputs, list)
                else len(inputs) if isinstance(inputs, str) else None
            )
        headers = {k: v for k, v in request.headers.items() if k.lower() not in HOP}
        headers.pop("Authorization", None)  # the LAN servers need no key
        self.inflight += 1
        entry["inflight_at_start"] = self.inflight
        captured: list[bytes] | None = [] if self.capture and kind == "chat" else None
        try:
            async with self.session.post(f"{base}{path}", data=raw, headers=headers) as upstream:
                entry["status"] = upstream.status
                if not stream:
                    payload = await upstream.read()
                    if captured is not None:
                        captured.append(payload)
                    entry["ttfb"] = time.time() - entry["start"]
                    try:
                        usage = json.loads(payload).get("usage") or {}
                    except ValueError:
                        usage = {}
                    self._usage(entry, usage)
                    out = web.Response(
                        body=payload, status=upstream.status,
                        content_type=upstream.content_type or "application/json",
                    )
                    return out
                response = web.StreamResponse(status=upstream.status)
                response.content_type = upstream.content_type or "text/event-stream"
                await response.prepare(request)
                buffer = b""
                first = None
                entry["done"] = False
                async for chunk in upstream.content.iter_any():
                    if first is None:
                        first = time.time()
                    buffer += chunk
                    if captured is not None:
                        captured.append(chunk)
                    while b"\n\n" in buffer:
                        event, buffer = buffer.split(b"\n\n", 1)
                        if self._swallow(event, entry, client_wants_usage):
                            continue
                        await response.write(event + b"\n\n")
                if buffer:
                    await response.write(buffer)
                entry["ttfb"] = (first or time.time()) - entry["start"]
                await response.write_eof()
                return response
        except Exception as error:  # noqa: BLE001 - recorded, then re-raised as 502
            if entry.get("done"):
                # The client hung up after the stream's [DONE] (before the
                # terminating chunk): the request completed.
                entry["late_disconnect"] = repr(error)[:120]
                return web.Response(status=200)
            entry["error"] = repr(error)[:300]
            entry.setdefault("status", 502)
            return web.json_response({"error": {"message": repr(error)}}, status=502)
        finally:
            self.inflight -= 1
            entry["end"] = time.time()
            entry["latency"] = entry["end"] - entry["start"]
            if captured is not None:
                self.captured += 1
                target = self.capture_dir / f"{self.captured:04d}.json"
                target.write_text(json.dumps({"entry": entry, "request": body,
                                              "response": b"".join(captured).decode("utf-8", "replace")}))
                entry["capture"] = target.name
            self.record(entry)

    def _swallow(self, event: bytes, entry: dict, client_wants_usage: bool) -> bool:
        """Record the usage chunk; True when it must not reach the client."""
        for line in event.split(b"\n"):
            if not line.startswith(b"data:"):
                continue
            data = line[5:].strip()
            if data == b"[DONE]":
                entry["done"] = True
                return False
            try:
                obj = json.loads(data)
            except ValueError:
                return False
            usage = obj.get("usage")
            if usage:
                self._usage(entry, usage)
                if not obj.get("choices") and not client_wants_usage:
                    return True
            choices = obj.get("choices") or []
            if choices and isinstance(choices[0].get("delta"), dict):
                delta = choices[0]["delta"]
                if delta.get("reasoning") or delta.get("reasoning_content"):
                    entry["reasoning_streamed"] = True
        return False

    @staticmethod
    def _usage(entry: dict, usage: dict) -> None:
        entry["prompt_tokens"] = usage.get("prompt_tokens")
        entry["completion_tokens"] = usage.get("completion_tokens")
        details = usage.get("completion_tokens_details") or {}
        entry["reasoning_tokens"] = details.get("reasoning_tokens")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=18950)
    parser.add_argument("--chat", required=True)
    parser.add_argument("--embed", required=True)
    parser.add_argument("--log", required=True, type=Path)
    parser.add_argument("--capture-dir", type=Path)
    args = parser.parse_args()
    args.log.parent.mkdir(parents=True, exist_ok=True)
    router = Router(args.chat, args.embed, args.log)
    if args.capture_dir:
        args.capture_dir.mkdir(parents=True, exist_ok=True)
        router.capture_dir = args.capture_dir
    app = web.Application(client_max_size=256 * 1024 * 1024)
    app.on_startup.append(router.start)
    app.on_cleanup.append(router.stop)
    app.router.add_post("/_label", router.set_label)
    app.router.add_get("/v1/models", router.models)
    app.router.add_get("/{prefix:.*}/v1/models", router.models)
    app.router.add_post("/v1/chat/completions", router.proxy)
    app.router.add_post("/v1/embeddings", router.proxy)
    web.run_app(app, host=args.host, port=args.port, access_log=None, print=None)


if __name__ == "__main__":
    main()
