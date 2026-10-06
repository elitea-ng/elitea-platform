"""A deterministic, prompt-aware OpenAI-compatible stub.

The standalone stack's llm-mock answers every chat completion with
"MOCK: <prompt>", which is fine for transport tests and useless for a pipeline
that asks the model for JSON. This stub inspects what is being asked and
returns a plausible, well-formed answer of the right SHAPE — a WikiStructureSpec
for a structure request, markdown for a page request, prose otherwise.

It is not an LLM and does not pretend to be. The content is canned; what is
being proven is that the ported pipeline runs end to end and composes the
frozen artifact set. Every response is a pure function of the request, so a
run is reproducible.

Embeddings are deterministic too: a seeded hash-bucket projection, the same
idea as the P0 retrieval fixtures' StubEmbedder.

The cluster structure planner's naming prompts (one batched call per
section; per page and section-from-pages on its fallback path) get names
derived from the symbols they list, so a structure is not all fallback
names. One batched answer in five (sections whose symbol count is a
multiple of 5) leaves out the last page on purpose, which drives the
planner onto its multi-call fallback path. Every other request is answered
as before.

``LLM_STUB_RECORD=<path>`` appends each chat request body, one JSON line
per request in arrival order, to that file. The ADR-0026 structure parity
gate compares the Python engine's requests with the Rust engine's.
"""
import hashlib, json, math, os, re, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DIM = 1536

STRUCTURE = {
    "wiki_title": "notes-service",
    "overview": "A small notes service with SQLite persistence, ranked search and bearer-token auth.",
    "total_pages": 3,
    "sections": [
        {
            "section_name": "Overview", "section_order": 0,
            "description": "What the service is and how a request flows through it.",
            "rationale": "Readers need orientation before any component detail.",
            "pages": [
                {"page_name": "Getting Started", "page_order": 0,
                 "description": "What the notes service does.",
                 "content_focus": "purpose and entry points",
                 "rationale": "First contact for a new reader.",
                 "target_symbols": ["handle_create_note", "handle_search"],
                 "key_files": ["api.py", "README.md"], "retrieval_query": "notes service overview"},
            ],
        },
        {
            "section_name": "Components", "section_order": 1,
            "description": "The three modules and what each owns.",
            "rationale": "Each module has a distinct responsibility worth its own page.",
            "pages": [
                {"page_name": "Note Storage", "page_order": 0,
                 "description": "How notes are persisted.",
                 "content_focus": "NoteStore and the SQLite schema",
                 "rationale": "Persistence is the core of the service.",
                 "target_symbols": ["NoteStore", "save_note", "load_note", "delete_note"],
                 "key_files": ["notes/store.py"], "retrieval_query": "how are notes stored"},
                {"page_name": "Bearer Tokens", "page_order": 1,
                 "description": "How requests are authenticated.",
                 "content_focus": "issue_token and verify_token",
                 "rationale": "Auth gates every write.",
                 "target_symbols": ["issue_token", "verify_token"],
                 "key_files": ["auth/tokens.py"], "retrieval_query": "verify bearer token signature"},
            ],
        },
    ],
}


def embed(text: str) -> list:
    vector = [0.0] * DIM
    for token in re.findall(r"[A-Za-z][A-Za-z0-9]+", text.lower()):
        digest = hashlib.sha256(f"deepwiki-e2e:{token}".encode()).hexdigest()
        vector[int(digest[:8], 16) % DIM] += 1.0
    norm = math.sqrt(sum(v * v for v in vector)) or 1.0
    return [v / norm for v in vector]


def _section_of(prompt: str, start: str, end: str):
    """The JSON value between two markers of a naming prompt, or None."""
    at = prompt.find(start)
    if at < 0:
        return None
    rest = prompt[at + len(start):]
    stop = rest.find(end)
    if stop < 0:
        return None
    try:
        return json.loads(rest[:stop])
    except ValueError:
        return None


def _page_name(symbols) -> str:
    names = [str(s.get("name", "")) for s in symbols if isinstance(s, dict)][:2]
    names = [n for n in names if n]
    return "Working with " + " and ".join(names) if names else "General Utilities"


def naming_answer(prompt: str):
    """Answers for the cluster planner's naming prompts, or None."""
    if "PAGES IN THIS SECTION:" in prompt and '"page_id"' in prompt:
        pages = _section_of(prompt, "PAGES IN THIS SECTION:\n", "\n\nOutput ONLY valid JSON")
        if not isinstance(pages, list):
            return None
        count = re.search(r"SECTION CLUSTER \u2014 (\d+) symbols", prompt)
        node_count = int(count.group(1)) if count else 0
        named = []
        for page in pages:
            if not isinstance(page, dict):
                continue
            symbols = page.get("page_symbols")
            symbols = symbols if isinstance(symbols, list) else []
            name = _page_name(symbols)
            named.append({
                "page_id": page.get("page_id"),
                "page_name": name,
                "description": f"How {name.lower()} works ({page.get('symbol_count')} symbols).",
                "retrieval_query": " ".join(
                    str(s.get("name", "")) for s in symbols[:4] if isinstance(s, dict)
                ),
            })
        if node_count % 5 == 0 and len(named) > 1:
            named = named[:-1]
        first = named[0]["page_name"] if named else "Overview"
        return json.dumps({
            "section_name": f"Area: {first}",
            "section_description": f"Capabilities around {first.lower()}.",
            "pages": named,
        })
    if "PAGE CLUSTER \u2014" in prompt and "PAGE SYMBOLS" in prompt:
        symbols = _section_of(prompt, "PAGE SYMBOLS (this page's own representative code elements):\n", "\n\nDIRECTORIES:")
        name = _page_name(symbols if isinstance(symbols, list) else [])
        return "```json\n" + json.dumps({
            "page_name": name,
            "description": f"Single-page naming for {name.lower()}.",
            "retrieval_query": name.lower(),
        }) + "\n```"
    if "SECTION (derived from" in prompt:
        pages = _section_of(prompt, "PAGES IN THIS SECTION:\n", "\n\nOutput ONLY valid JSON")
        first = (
            pages[0].get("page_name", "Overview")
            if isinstance(pages, list) and pages and isinstance(pages[0], dict)
            else "Overview"
        )
        return json.dumps({
            "section_name": f"Domain of {first}",
            "section_description": "Derived from its pages.",
        })
    if "SECTION CLUSTER \u2014" in prompt and "TOP SYMBOLS" in prompt:
        return json.dumps({
            "section_name": "Core Section",
            "section_description": "Named from its top symbols.",
        })
    return None


def answer(prompt: str) -> str:
    named = naming_answer(prompt)
    if named is not None:
        return named
    lowered = prompt.lower()
    wants_json = "json" in lowered or "wiki_title" in lowered or "sections" in lowered
    if wants_json and ("structure" in lowered or "sections" in lowered or "wiki_title" in lowered):
        return json.dumps(STRUCTURE)
    if wants_json:
        # Repository analysis and other JSON asks: a generic well-formed object.
        return json.dumps({
            "executive_summary": "A small notes service: store, search and authorise notes.",
            "core_purpose": "Persist notes and answer ranked searches over them.",
            "key_components": ["NoteStore", "rank_notes", "verify_token"],
            "architecture": "A thin HTTP layer over a SQLite store and an in-memory index.",
        })
    return (
        "## Overview\n\n"
        "The notes service stores notes in SQLite, ranks search results by term "
        "overlap, and authenticates writes with signed bearer tokens.\n\n"
        "```mermaid\nflowchart LR\n  api --> store\n  api --> auth\n```\n\n"
        "See `notes/store.py` for persistence and `auth/tokens.py` for signing.\n"
    )


_RECORD_LOCK = threading.Lock()


def record(request) -> None:
    """Append one chat request body to ``LLM_STUB_RECORD`` (when set)."""
    path = os.environ.get("LLM_STUB_RECORD")
    if not path:
        return
    with _RECORD_LOCK, open(path, "a", encoding="utf-8") as handle:
        handle.write(json.dumps(request, ensure_ascii=False, sort_keys=True) + "\n")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_a):
        pass

    def _send(self, payload):
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.endswith("/models"):
            self._send({"object": "list", "data": [
                {"id": "gpt-4o", "object": "model", "owned_by": "stub"},
                {"id": "text-embedding-3-small", "object": "model", "owned_by": "stub"},
            ]})
        else:
            self.send_response(404); self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("content-length", 0))
        request = json.loads(self.rfile.read(length) or b"{}")

        if self.path.endswith("/embeddings"):
            inputs = request.get("input")
            if isinstance(inputs, str):
                inputs = [inputs]
            self._send({"object": "list", "model": request.get("model", "stub"),
                        "data": [{"object": "embedding", "index": i, "embedding": embed(str(t))}
                                 for i, t in enumerate(inputs or [""])],
                        "usage": {"prompt_tokens": 1, "total_tokens": 1}})
            return

        if self.path.endswith("/chat/completions"):
            record(request)
            prompt = "\n".join(
                str(m.get("content", "")) for m in request.get("messages", [])
            )
            content = answer(prompt)

            # The subprocess workers build their LLM with streaming=True and
            # fail with "No generations found in stream" against a
            # non-streaming endpoint, so the stub speaks SSE too.
            if request.get("stream"):
                self.send_response(200)
                self.send_header("content-type", "text/event-stream")
                self.end_headers()
                base = {"id": "chatcmpl-stub", "object": "chat.completion.chunk",
                        "created": 0, "model": request.get("model", "stub")}
                for start in range(0, len(content), 400):
                    chunk = dict(base, choices=[{
                        "index": 0, "finish_reason": None,
                        "delta": {"content": content[start:start + 400]}}])
                    self.wfile.write(b"data: " + json.dumps(chunk).encode() + b"\n\n")
                final = dict(base, choices=[
                    {"index": 0, "finish_reason": "stop", "delta": {}}])
                self.wfile.write(b"data: " + json.dumps(final).encode() + b"\n\n")
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
                return

            self._send({"id": "chatcmpl-stub", "object": "chat.completion",
                        "created": 0, "model": request.get("model", "stub"),
                        "choices": [{"index": 0, "finish_reason": "stop",
                                     "message": {"role": "assistant", "content": content}}],
                        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
            return

        self.send_response(404); self.end_headers()


if __name__ == "__main__":
    # Loopback by default (the manual harness); a compose service binds all
    # interfaces and takes the mock's port so the gateway's egress allowlist
    # (`llm-mock:8090`) needs no change.
    ThreadingHTTPServer(
        (os.environ.get("LLM_STUB_HOST", "127.0.0.1"), int(os.environ.get("LLM_STUB_PORT", "18901"))),
        Handler,
    ).serve_forever()
