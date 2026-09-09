"""What this worker IMAGE can do with the platform's internal-tool catalogue.

The platform forwards the authored `meta.internal_tools` set as it stands and
lets each runtime decide (`currentRuntimeInternalTools` in
services/elitea-main/internal/application/agentexecution/start.go). The native
Rust runtime answers that by SKIPPING what it does not implement, with a logged
`agent_internal_tool_skipped`
(services/elitea-worker-rust/src/agents/internal_tools.rs). This module is the
Python worker's half of the same contract, and it exists because "the Python
worker serves the whole set" was written down in three places and was not true.

MEASURED, on the image this directory builds, with every switch on the agent
form toggled on:

  * `planner`. `EliteAClient.application()` constructs a `PlanningMiddleware`
    the moment `planner` appears in `meta.internal_tools`, always with
    `connection_string=None`, so its `PlanningWrapper` falls back to
    `FilesystemStorage`, whose base directory defaults to `$ELITEA_DIR/plans`
    and, with no `ELITEA_DIR`, to the RELATIVE path `.elitea/plans`. The worker
    container has no WORKDIR, so that resolves against `/`, which uid 10001
    cannot write:

      {"event":"agent_execution_internal_failure","exception_name":"PermissionError",
       "frames":[…{"file":"wrapper.py","function":"setup_storage"},
                 {"file":"wrapper.py","function":"__init__"},
                 {"file":"pathlib.py","function":"mkdir"}],"stage":"execute"}

    That is not a missing capability, it is a missing directory, so it is FIXED
    rather than skipped: `ensure_sdk_state_directory` points `ELITEA_DIR` at a
    private directory the worker can write in every deployment shape this
    repository ships — compose (writable image filesystem) and Kubernetes
    (`readOnlyRootFilesystem: true` plus a `/tmp` emptyDir,
    deploy/helm/elitea/templates/worker/deployment.yaml).

  * `pyodide`. The SDK materialises it through `SandboxToolkit.get_toolkit`,
    whose tool constructor (`BasePyodideSandbox.__init__`) raises unless THREE
    things are true: `deno` is on PATH, `PYODIDE_SANDBOX_PKG` names a Deno
    entrypoint script that exists, and — MEASURED, past both of those — the
    worker's own process CWD is writable, because the entrypoint hardcodes
    `node_modules_dir="auto"` and deno then materialises a `node_modules`
    directory relative to CWD on every run. As of #872 the image fixes all
    three:

      - a pinned, checksum-verified `deno` binary (the Containerfile and
        `scripts/install_pinned_deno.py`);
      - the SDK's own patched entrypoint, `infra/data/sandbox/main.ts` at the
        pinned SDK revision, baked into the image at
        `_DEFAULT_PYODIDE_SANDBOX_PKG` — its content needs no separate pin,
        being already covered by `ELITEA_SDK_ARCHIVE_SHA256` — together with a
        `deno cache` of its whole module graph (`npm:pyodide@0.29.0` plus two
        `jsr:@std` packages, all exact-versioned) pre-warmed at build time so
        no later `deno run` of it touches the network;
      - `ensure_sandbox_state_directories` points `SANDBOX_BASE`/`DENO_DIR` at
        directories this worker can write when it owns that choice (the image
        instead bakes `DENO_DIR`/`PYODIDE_SANDBOX_PKG` itself — same class of
        fix `ensure_sdk_state_directory` applies to `ELITEA_DIR`/`planner`),
        and `_prepare_sandbox_run_directory` gives the process a writable CWD
        before deno's own `node_modules` materialisation, which is a step
        beyond what `ELITEA_DIR` needed.

WHY SKIPPING AND NOT REFUSING. The whole turn dies on either failure — the
exception escapes `LangChainAssistant`'s constructor, and the browser gets an
assistant row flagged `is_error` with EMPTY content, naming neither the toggle
nor the runtime. An agent that answers without one tool is a smaller loss than
an agent that stops answering, and the skip is logged so an operator can see
which capability the deployment is not providing.

WHY NOT A WIDER TRY/EXCEPT. The construction that fails happens inside the
SDK's `get_tools()`, which builds every tool for the run in one pass; there is
no seam there this worker owns. Pruning the REQUEST is the lever the worker
does own, and it is exact: the name never reaches the SDK, so nothing partial
is built.

A name this module does not know is left alone. `attachments`, `internal_mcp`,
`swarm`, `data_analysis` and `lazy_tools_mode` all reach the SDK today and
either configure a mode or resolve to no tool; only a name with a measured,
image-level precondition belongs in `_PRECONDITIONS`.
"""

from __future__ import annotations

import json
import os
import shutil
import sys
import tempfile
from collections.abc import Iterable
from typing import Callable

__all__ = [
    "ensure_sandbox_state_directories",
    "ensure_sdk_state_directory",
    "serve_internal_tools",
    "unservable_internal_tools",
]


# The directory name is this worker's, not the SDK's: `ELITEA_DIR` is also the
# SDK CLI's configuration root, and a shared name would let a developer's
# `.elitea` tree and a worker's plan spool mean the same path.
_STATE_DIRECTORY_NAME = "elitea-worker-state"
# The sandbox's two cache directories live under the same private tree, in
# their own named subdirectories, so a single readable-only-by-this-uid
# directory covers both `planner` and `pyodide` state.
_SANDBOX_BASE_DIRECTORY_NAME = "pyodide-sandbox"
_DENO_CACHE_DIRECTORY_NAME = "deno-cache"

# Where the Containerfile bakes the SDK's patched Deno entrypoint
# (infra/data/sandbox/main.ts at the pinned SDK revision — its content is
# already covered by ELITEA_SDK_ARCHIVE_SHA256, so nothing here pins it a
# second time) and a fully pre-warmed `deno cache` of its module graph
# (npm:pyodide + two jsr:@std packages, all exact-versioned specifiers). Used
# only when the operator has not set PYODIDE_SANDBOX_PKG/DENO_DIR themselves.
_DEFAULT_PYODIDE_SANDBOX_PKG = "/opt/elitea/sandbox/main.ts"

_SANDBOX_TOOL = "pyodide"
_SANDBOX_REASON = "sandbox_backend_unavailable"


def _pyodide_entrypoint_path() -> str:
    """The Deno script `PyodideSandboxTool` will run, resolved the same way
    `elitea_sdk.runtime.langchain.pyodide_sandbox.get_default_pkg_name` does.
    """

    configured = (os.environ.get("PYODIDE_SANDBOX_PKG") or "").strip()
    return configured or _DEFAULT_PYODIDE_SANDBOX_PKG


def _sandbox_backend_available() -> bool:
    """Mirror the SDK's own preconditions for a Python sandbox.

    `PyodideSandboxTool._initialize_sandbox` takes a remote sandbox when
    `SANDBOX_SERVICE_URL` is set. Otherwise `BasePyodideSandbox.__init__`
    requires BOTH a non-empty `pkg_name` (from `PYODIDE_SANDBOX_PKG`, or the
    image's own default) that names a file that actually exists, AND `deno` on
    PATH. All three are read here rather than assumed, so an image or a
    deployment that adds any one of them starts serving the tool without
    another change in this file.
    """

    if (os.environ.get("SANDBOX_SERVICE_URL") or "").strip():
        return True
    if shutil.which("deno") is None:
        return False
    return os.path.isfile(_pyodide_entrypoint_path())


# name -> (precondition, reason_code). Only names with a MEASURED, image-level
# precondition belong here; see the module docstring.
_PRECONDITIONS: dict[str, tuple[Callable[[], bool], str]] = {
    _SANDBOX_TOOL: (_sandbox_backend_available, _SANDBOX_REASON),
}


def ensure_sdk_state_directory() -> str:
    """Point the SDK's filesystem state at a directory this worker can write.

    Idempotent, and callable on every run: it reads one environment variable and
    creates one directory with `exist_ok`.

    An `ELITEA_DIR` the operator set is left exactly as it is, including one
    that turns out to be unwritable. A deployment that names a directory has
    made a choice about where conversation-derived state lands, and silently
    relocating it would be worse than the error it would hide.
    """

    configured = (os.environ.get("ELITEA_DIR") or "").strip()
    if configured:
        return configured

    directory = os.path.join(tempfile.gettempdir(), _STATE_DIRECTORY_NAME)
    # 0700 twice: `makedirs` masks its mode with the process umask, so the
    # explicit chmod is what actually guarantees it. The files underneath carry
    # plan titles and step descriptions the model wrote, which is conversation
    # content and not something a second uid on the host should be able to read.
    os.makedirs(directory, mode=0o700, exist_ok=True)
    os.chmod(directory, 0o700)
    os.environ["ELITEA_DIR"] = directory
    return directory


def _private_directory(path: str) -> str:
    """Create `path` (and parents) as a directory only this uid can read."""

    os.makedirs(path, mode=0o700, exist_ok=True)
    os.chmod(path, 0o700)
    return path


def ensure_sandbox_state_directories() -> tuple[str, str]:
    """Point the SDK's Pyodide sandbox cache directories at ones this worker
    can write, and create them.

    `elitea_sdk.runtime.tools.sandbox._initialize_sandbox` reads `SANDBOX_BASE`
    and `DENO_DIR` and falls back to `~/.cache/pyodide` and `~/.cache/deno`
    when either is unset. This worker's image user has `--home-dir
    /nonexistent`, so both defaults resolve under a path uid 10001 cannot
    create — the same class of bug `ensure_sdk_state_directory` exists to fix
    for `ELITEA_DIR`/`planner`.

    Idempotent, and callable on every run. A `SANDBOX_BASE` or `DENO_DIR` the
    OPERATOR set is left exactly as it is — nothing here even stats it, let
    alone creates a subdirectory under it — for the same reason `ELITEA_DIR`
    is: including one that turns out to be unwritable, naming a directory is
    a choice about where sandbox state lands. (`_prepare_sandbox_run_
    directory` still creates `<SANDBOX_BASE>/tmp` on demand, right before the
    one thing that needs it, regardless of who chose the parent — seeing
    this module's OWN default is not the only way that directory can be
    missing.)

    `DENO_DIR`/`PYODIDE_SANDBOX_PKG` are usually already set here: the image
    bakes both at a fully pre-warmed, read-only path (see the Containerfile),
    which reads as "the deployment already chose", the same as an operator's
    own value.
    """

    sandbox_base = (os.environ.get("SANDBOX_BASE") or "").strip()
    if not sandbox_base:
        sandbox_base = os.path.join(
            tempfile.gettempdir(), _STATE_DIRECTORY_NAME, _SANDBOX_BASE_DIRECTORY_NAME
        )
        _private_directory(sandbox_base)
        os.environ["SANDBOX_BASE"] = sandbox_base

    deno_dir = (os.environ.get("DENO_DIR") or "").strip()
    if not deno_dir:
        deno_dir = os.path.join(
            tempfile.gettempdir(), _STATE_DIRECTORY_NAME, _DENO_CACHE_DIRECTORY_NAME
        )
        _private_directory(deno_dir)
        os.environ["DENO_DIR"] = deno_dir

    if not (os.environ.get("PYODIDE_SANDBOX_PKG") or "").strip():
        # Read directly by `elitea_sdk...pyodide_sandbox.get_default_pkg_name`;
        # setting it here (rather than only in `_pyodide_entrypoint_path`)
        # means a caller that reads the environment directly, not through this
        # module, still sees the image's own entrypoint.
        os.environ["PYODIDE_SANDBOX_PKG"] = _DEFAULT_PYODIDE_SANDBOX_PKG

    return sandbox_base, deno_dir


def _prepare_sandbox_run_directory() -> None:
    """Give the process a writable, already-permitted current directory
    before Pyodide runs.

    Every Pyodide sandbox tool the SDK builds hardcodes `node_modules_dir=
    "auto"`, which makes deno materialise a `node_modules` tree under CWD on
    EVERY run — MEASURED, even when `DENO_DIR`'s own module cache is already
    fully pre-warmed (the Containerfile's build-time `deno cache` step) and
    read-only at runtime: deno's own dependency materialisation runs with
    deno's full privilege, outside the script's `--allow-write` grants, so it
    succeeds into an unwritable CWD too — but the SANDBOXED SCRIPT then fails
    to `--allow-read` the files deno just wrote there, because CWD is not one
    of the three roots `_initialize_sandbox` grants
    (`sandbox_base`/`sandbox_base/tmp`/`DENO_DIR`).

    This worker's process CWD is `/` in every deployment shape this repository
    ships (no WORKDIR is set), which is neither writable NOR one of those
    three granted roots. Changing CWD to `<SANDBOX_BASE>/tmp` — a directory
    the SDK already grants both read AND write on — fixes both problems with
    the one existing, already-permitted directory, rather than adding a
    fourth path the SDK would need to be told about and cannot be. Created
    here, on demand, rather than relying on `ensure_sandbox_state_directories`
    to have created it: that function does not touch an operator-chosen
    `SANDBOX_BASE` at all, and this directory is an SDK-internal convention —
    not a location the operator named — so creating it here is not the same
    kind of relocation `ensure_sandbox_state_directories` avoids.

    Scoped to run only for a turn that actually keeps `pyodide` in its served
    set AND is not using a remote sandbox service (see the one call site, in
    `serve_internal_tools`) — a remote sandbox spawns no local deno subprocess
    and needs no local CWD — rather than changing the process's CWD
    unconditionally on every turn: nothing else in this worker is known to
    depend on CWD, but there is no `cwd=` seam this worker owns in the SDK's
    subprocess call to scope the change any more tightly than that.
    """

    sandbox_base = (os.environ.get("SANDBOX_BASE") or "").strip()
    if not sandbox_base:
        # ensure_sandbox_state_directories always runs first and always sets
        # this; an empty value here means it did not, which is a caller bug.
        raise RuntimeError("SANDBOX_BASE is unset; call ensure_sandbox_state_directories first")
    sandbox_tmp = os.path.join(sandbox_base, "tmp")
    _private_directory(sandbox_tmp)
    os.chdir(sandbox_tmp)


def unservable_internal_tools(names: Iterable[str]) -> list[tuple[str, str]]:
    """The (name, reason_code) pairs this image cannot materialise, in order."""

    unservable: list[tuple[str, str]] = []
    for name in names:
        precondition = _PRECONDITIONS.get(name)
        if precondition is None:
            continue
        available, reason = precondition
        if not available():
            unservable.append((name, reason))
    return unservable


def serve_internal_tools(names: Iterable[str]) -> list[str]:
    """Return the subset the SDK can build here, reporting what was dropped.

    Order and duplicates are preserved: the platform already de-duplicated and
    ordered this list, and reordering it here would make two runs of the same
    agent send two different requests.
    """

    requested = list(names)
    dropped = dict(unservable_internal_tools(requested))
    served = requested if not dropped else [
        name for name in requested if name not in dropped
    ]
    if dropped:
        for name, reason in dropped.items():
            _report_skipped_internal_tool(name, reason)
    # A remote sandbox service spawns no local deno subprocess, so it has no
    # use for a local writable CWD; only the local-deno path needs it.
    if _SANDBOX_TOOL in served and not (os.environ.get("SANDBOX_SERVICE_URL") or "").strip():
        _prepare_sandbox_run_directory()
    return served


def _report_skipped_internal_tool(name: str, reason: str) -> None:
    """One operator-facing line per skipped tool, in the runtime's own shape.

    Same event name and fields as the Rust runtime's `tracing::warn!`, so a
    deployment running either worker is grepped the same way. It carries no
    execution content — the tool name is platform vocabulary, not user input.
    """

    print(
        json.dumps(
            {
                "event": "agent_internal_tool_skipped",
                "internal_tool": name,
                "reason_code": reason,
            },
            sort_keys=True,
            separators=(",", ":"),
        ),
        file=sys.stderr,
        flush=True,
    )
