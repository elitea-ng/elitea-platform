#!/usr/bin/env python3
"""Run one DeepWiki engine on one corpus, as the Go host drives it.

The engine is started through its real entry point, on a Unix socket, and
invoked with the legacy keyword arguments the host derives
(``services/elitea-subapp-host/internal/apps/deepwiki/run/params.go``
``ArgumentsFor``):

* ``rust``            ``elitea-deepwiki-engine serve``, ``ELITEA_DEEPWIKI_RUNNER=native``,
                      PostgreSQL + pgvector; ``generate_wiki`` runs in the
                      worker child, ``ask`` in process over PostgreSQL.
* ``python-shipped``  ``python -m elitea_deepwiki``, ``ELITEA_DEEPWIKI_RUNNER=legacy``,
                      no database (the SQLite ``.wiki.db`` index in the scratch
                      cache); ``generate_wiki`` with ``run_in_subprocess=True``
                      runs ``wiki_subprocess_worker`` in a child process,
                      ``ask`` runs ``ask_subprocess_worker`` in a child process.
* ``python-patched``  the same, with ``parity/bench/pypatch`` on PYTHONPATH and
                      ``DWB_EMBED_DIM=2560``: ``UnifiedWikiDB``'s vector
                      dimension is the model's instead of ``1536``.

Measured: cold start (spawn to ``GET /engine/health`` answering), the
``generate_wiki`` wall time and its timestamped progress lines, every ``ask``
of the subset (wall time, answer), the peak RSS of the engine's process TREE
(``ps`` every 0.25 s, summed over the server and its descendants, per phase)
and ``/usr/bin/time -l``'s maximum resident set size of the server (which
on macOS includes reaped children). The model router (``model_router.py``)
gets a label per phase and per question, so its log separates model time.

Writes ``<out>/run.json`` (metrics), ``<out>/generate_lines.jsonl``,
``<out>/result.json`` (the result, artifact data removed),
``<out>/artifacts/...`` (every artifact's data), ``<out>/ask.jsonl``.
"""

from __future__ import annotations

import argparse
import http.client
import json
import os
import signal
import socket
import subprocess
import threading
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
ENGINE_DIR = HERE.parent.parent
SERVICES = ENGINE_DIR.parent
CHAT_MODEL = "RadixArk/Qwen3.8-27B-NVFP4"
EMBED_MODEL = "Qwen/Qwen3-Embedding-4B"


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path: str, timeout: float | None = None):
        super().__init__("engine", timeout=timeout)
        self._path = path

    def connect(self) -> None:
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        if self.timeout:
            self.sock.settimeout(self.timeout)
        self.sock.connect(self._path)


def label(router: str, text: str) -> None:
    capture = os.environ.get("DWB_CAPTURE") == "1"
    request = urllib.request.Request(
        f"{router}/_label", data=json.dumps({"label": text, "capture": capture}).encode(),
        headers={"content-type": "application/json"},
    )
    urllib.request.urlopen(request, timeout=5).read()


def tree_rss_kb(root: int) -> tuple[int, int]:
    """(sum of RSS in KiB over root and its descendants, process count)."""
    out = subprocess.run(["ps", "-A", "-o", "pid=,ppid=,rss="], capture_output=True, text=True).stdout
    children: dict[int, list[int]] = {}
    rss: dict[int, int] = {}
    for line in out.splitlines():
        parts = line.split()
        if len(parts) != 3:
            continue
        pid, ppid, kb = map(int, parts)
        children.setdefault(ppid, []).append(pid)
        rss[pid] = kb
    total, count, stack = 0, 0, [root]
    while stack:
        pid = stack.pop()
        if pid in rss:
            total += rss[pid]
            count += 1
        stack.extend(children.get(pid, []))
    return total, count


class Sampler(threading.Thread):
    def __init__(self, root: int, path: Path):
        super().__init__(daemon=True)
        self.root, self.phase, self.stop = root, "start", threading.Event()
        self.peaks: dict[str, int] = {}
        self.file = path.open("w")

    def run(self) -> None:
        while not self.stop.is_set():
            kb, count = tree_rss_kb(self.root)
            self.peaks[self.phase] = max(self.peaks.get(self.phase, 0), kb)
            self.file.write(f"{time.time():.2f} {self.phase} {kb} {count}\n")
            self.stop.wait(0.25)
        self.file.close()


def engine_env(engine: str, args, scratch: Path, sock: Path) -> tuple[list[str], dict]:
    env = dict(os.environ)
    env.update({
        "ELITEA_DEEPWIKI_SCRATCH_PATH": str(scratch),
        "ELITEA_DEEPWIKI_ENGINE_SOCKET": str(sock),
        "ELITEA_DEEPWIKI_GIT_ALLOWLIST": "github.com",
    })
    if engine == "rust":
        env.update({
            "ELITEA_DEEPWIKI_RUNNER": "native",
            "ELITEA_DEEPWIKI_DATABASE_URL": args.dsn,
            "ELITEA_DEEPWIKI_BUILD_OWNER": f"dw-bench-{os.getpid()}",
            "RUST_LOG": "info",
        })
        return [args.rust_bin, "serve"], env
    env.pop("ELITEA_DEEPWIKI_DATABASE_URL", None)
    env["ELITEA_DEEPWIKI_RUNNER"] = "legacy"
    # The Python engine image's default (services/elitea-deepwiki/Containerfile):
    # without it ask takes the classic FAISS path, whose index the indexer no
    # longer builds, and every question answers "No wiki index found".
    env["DEEPWIKI_ASK_AGENTIC"] = "1"
    env["PYTHONHASHSEED"] = "0"
    paths = [str(SERVICES / "elitea-deepwiki" / "src")]
    if engine == "python-patched":
        paths.insert(0, str(HERE / "pypatch"))
        env["DWB_EMBED_DIM"] = str(args.embed_dim)
    else:
        env.pop("DWB_EMBED_DIM", None)
    env["PYTHONPATH"] = os.pathsep.join(paths)
    return [args.python, "-m", "elitea_deepwiki"], env


def health(sock: Path) -> dict | None:
    try:
        conn = UnixHTTPConnection(str(sock), timeout=2)
        conn.request("GET", "/engine/health")
        response = conn.getresponse()
        body = response.read()
        conn.close()
        return json.loads(body) if response.status == 200 else None
    except OSError:
        return None


def invoke(sock: Path, invocation: str, tool: str, arguments: dict, lines_out) -> tuple[list, float]:
    started = time.time()
    conn = UnixHTTPConnection(str(sock), timeout=None)
    body = json.dumps({"invocation_id": invocation, "tool": tool, "arguments": arguments})
    conn.request("POST", "/engine/invoke", body=body, headers={"content-type": "application/json"})
    response = conn.getresponse()
    if response.status != 200:
        raise RuntimeError(f"{tool}: HTTP {response.status} {response.read()[:500]!r}")
    lines = []
    while True:
        raw = response.readline()
        if not raw:
            break
        if not raw.strip():
            continue
        item = json.loads(raw)
        stamp = time.time() - started
        lines.append((stamp, item))
        if lines_out is not None and "token" not in item:
            lines_out.write(json.dumps({"t": round(stamp, 3), **{k: v for k, v in item.items() if k != "result"}}) + "\n")
            lines_out.flush()
    conn.close()
    return lines, time.time() - started


def arguments_for(tool: str, args, question: str | None = None) -> dict:
    llm_settings = {
        "model_name": CHAT_MODEL,
        "api_base": f"{args.router}/v1", "api_key": "bench",
        "openai_api_base": f"{args.router}/v1", "openai_api_key": "bench",
        "organization": "1",
    }
    repo_config = {
        "provider_type": "github",
        "provider_config": {"base_url": "https://api.github.com"},
        "repository": args.repo, "branch": args.branch, "project": None, "is_cloud": None,
    }
    common = {"llm_settings": llm_settings, "embedding_model": EMBED_MODEL}
    if tool == "generate_wiki":
        return {**common, "query": f"Document the {args.repo} repository", "repo_config": repo_config,
                "active_branch": args.branch, "force_rebuild_index": True,
                "indexing_method": "filesystem", "planner_mode": "cluster",
                "exclude_tests": None, "run_in_subprocess": True}
    return {**common, "question": question, "repo_config": repo_config, "chat_history": [],
            "k": 15, "repo_identifier_override": None, "analysis_key_override": None}


def save_result(result: dict, out: Path) -> dict:
    artifacts = result.get("artifacts") or []
    summary = []
    for artifact in artifacts:
        name = artifact.get("name") or "unnamed"
        data = artifact.get("data") or ""
        target = out / "artifacts" / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(data if isinstance(data, str) else json.dumps(data), encoding="utf-8")
        summary.append({"name": name, "type": artifact.get("type"), "bytes": len(data)})
    slim = {k: v for k, v in result.items() if k != "artifacts"}
    slim["artifacts"] = summary
    (out / "result.json").write_text(json.dumps(slim, indent=1), encoding="utf-8")
    return slim


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("engine", choices=["rust", "python-shipped", "python-patched"])
    parser.add_argument("corpus")
    parser.add_argument("--repo", required=True)
    parser.add_argument("--branch", required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--questions", type=Path)
    parser.add_argument("--ask", type=int, default=15, help="asks from the subset (0: none)")
    parser.add_argument("--skip-generate", action="store_true")
    parser.add_argument("--router", default="http://127.0.0.1:18950")
    parser.add_argument("--dsn", default="postgres://deepwiki:deepwiki@127.0.0.1:15438/deepwiki")
    parser.add_argument("--rust-bin", default=str(Path.home() / ".cache/elitea-cargo-target-bench/release/elitea-deepwiki-engine"))
    parser.add_argument("--python", default=str(Path.home() / ".cache/elitea-dw-parity/pyengine/bin/python"))
    # NOT under a hidden directory: the Python engine's file discovery skips
    # every path with a dot component, the scratch root's included, and
    # indexes nothing from a clone under ~/.cache (an empty graph).
    parser.add_argument("--scratch", type=Path, default=Path("/tmp/dwb-scratch"))
    parser.add_argument("--embed-dim", type=int, default=2560)
    parser.add_argument("--timeout", type=float, default=4 * 3600)
    args = parser.parse_args()

    out = args.out
    out.mkdir(parents=True, exist_ok=True)
    scratch = args.scratch / args.engine / args.corpus
    scratch.mkdir(parents=True, exist_ok=True)
    sock = Path(f"/tmp/dwb-{args.engine[:2]}-{args.corpus[:6]}.sock")
    if sock.exists():
        sock.unlink()
    command, env = engine_env(args.engine, args, scratch, sock)
    metrics: dict = {"engine": args.engine, "corpus": args.corpus, "repo": args.repo, "branch": args.branch,
                     "command": command}
    log = (out / "server.log").open("ab")
    timing = out / "time_l.txt"
    started = time.time()
    server = subprocess.Popen(["/usr/bin/time", "-l", "-o", str(timing), *command], env=env,
                              stdout=log, stderr=subprocess.STDOUT, cwd=str(SERVICES.parent),
                              start_new_session=True)
    sampler = Sampler(server.pid, out / "rss_samples.txt")
    sampler.start()
    try:
        while True:
            state = health(sock)
            if state is not None:
                break
            if server.poll() is not None:
                raise RuntimeError(f"server exited with {server.returncode}; see {out / 'server.log'}")
            if time.time() - started > 120:
                raise RuntimeError("no health answer within 120 s")
            time.sleep(0.02)
        metrics["cold_start_s"] = time.time() - started
        metrics["health"] = state
        sampler.phase = "idle"
        time.sleep(1.0)

        if not args.skip_generate:
            sampler.phase = "generate"
            label(args.router, f"{args.engine}/{args.corpus}/generate")
            with (out / "generate_lines.jsonl").open("w") as lines_out:
                t0 = time.time()
                lines, wall = invoke(sock, f"gen-{args.corpus}-{int(t0)}", "generate_wiki",
                                     arguments_for("generate_wiki", args), lines_out)
            metrics["generate"] = {"start": t0, "end": t0 + wall, "wall_s": wall}
            last = lines[-1][1] if lines else {}
            if "result" in last:
                slim = save_result(last["result"], out)
                metrics["generate"]["success"] = bool(slim.get("success"))
                metrics["generate"]["errors"] = slim.get("errors")
                metrics["generate"]["wiki_id"] = slim.get("wiki_id")
                metrics["generate"]["commit_hash"] = slim.get("commit_hash")
                metrics["generate"]["pages"] = sum(1 for a in slim["artifacts"] if a["type"] == "text/markdown")
            else:
                metrics["generate"]["success"] = False
                metrics["generate"]["error"] = last.get("error")
            sampler.phase = "after-generate"
            time.sleep(1.0)

        if args.ask and args.questions:
            questions = [json.loads(l) for l in args.questions.open() if l.strip()]
            subset = [q for q in questions if q.get("ask_subset")][: args.ask]
            sampler.phase = "ask"
            asks = []
            with (out / "ask.jsonl").open("w") as ask_out:
                for q in subset:
                    label(args.router, f"{args.engine}/{args.corpus}/ask/{q['id']}")
                    t0 = time.time()
                    try:
                        lines, wall = invoke(sock, f"ask-{q['id']}-{int(t0)}", "ask",
                                             arguments_for("ask", args, q["question"]), None)
                    except Exception as error:  # noqa: BLE001 - recorded
                        lines, wall = [(0.0, {"error": {"message": repr(error)}})], time.time() - t0
                    last = lines[-1][1] if lines else {}
                    tokens = "".join(item.get("token", "") for _, item in lines)
                    result = last.get("result") if isinstance(last.get("result"), dict) else {}
                    answer = result.get("answer") or tokens
                    first_token = next((stamp for stamp, item in lines if "token" in item), None)
                    entry = {"id": q["id"], "question": q["question"], "start": t0, "end": t0 + wall,
                             "wall_s": wall, "first_token_s": first_token,
                             "success": bool(result.get("success")), "answer": answer,
                             "error": last.get("error") or (None if result.get("success") else result.get("error")),
                             "thinking_lines": sum(1 for _, item in lines if "thinking" in item),
                             "thinking": [item["thinking"][:400] for _, item in lines if "thinking" in item]}
                    asks.append({k: entry[k] for k in ("id", "start", "end", "wall_s", "success")})
                    ask_out.write(json.dumps(entry, ensure_ascii=False) + "\n")
                    ask_out.flush()
                    print(f"  ask {q['id']}: {wall:.1f}s success={entry['success']}", flush=True)
            metrics["ask"] = asks
        label(args.router, "idle")
    finally:
        sampler.phase = "stop"
        # SIGTERM the engine (the child of /usr/bin/time), so time itself
        # exits normally and writes the rusage; the group is the fallback.
        engine_pids = subprocess.run(["pgrep", "-P", str(server.pid)], capture_output=True,
                                     text=True).stdout.split()
        for pid in engine_pids:
            try:
                os.kill(int(pid), signal.SIGTERM)
            except ProcessLookupError:
                pass
        try:
            server.wait(timeout=30)
        except subprocess.TimeoutExpired:
            os.killpg(server.pid, signal.SIGKILL)
            server.wait()
        try:
            os.killpg(server.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        sampler.stop.set()
        sampler.join()
        metrics["rss_peak_tree_kb"] = sampler.peaks
        if timing.exists():
            for line in timing.read_text().splitlines():
                if "maximum resident set size" in line:
                    metrics["time_l_max_rss_bytes"] = int(line.split()[0])
        (out / "run.json").write_text(json.dumps(metrics, indent=1))
        log.close()
    print(json.dumps({k: metrics.get(k) for k in ("engine", "corpus", "cold_start_s")} |
                     {"generate": {k: v for k, v in (metrics.get("generate") or {}).items() if k not in ("errors",)}},
                     default=str))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
