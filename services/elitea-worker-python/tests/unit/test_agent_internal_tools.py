"""The internal-tool set this worker image can actually build.

Both halves here were measured on the built image before they were written, by
toggling every switch on the agent form and sending one message. The turn died
in the SDK, and the browser got an assistant row flagged `is_error` with EMPTY
content — a failure that names neither the toggle nor the runtime:

    {"event":"agent_execution_internal_failure","exception_name":"PermissionError",
     "frames":[{"file":"sdk_adapter.py","function":"execute_application"},
               {"file":"client.py","function":"application"},
               {"file":"middleware.py","function":"__init__"},
               {"file":"wrapper.py","function":"setup_storage"},
               {"file":"wrapper.py","function":"__init__"},
               {"file":"pathlib.py","function":"mkdir"}],"stage":"execute"}

and, behind it, `RuntimeError: Deno is required for PyodideSandbox…`.

The distinction these tests protect is FIXED vs SKIPPED. `planner` failed for a
missing directory, which the worker can provide, so it must still reach the SDK.
`pyodide` failed for a missing sandbox backend this image does not ship, so it
must NOT reach the SDK — and it must be reported rather than dropped in silence,
because the agent then runs without a capability its author asked for.
"""

from __future__ import annotations

import json
import os
import shutil
from typing import Any

import pytest

from elitea_worker.agents import internal_tools, sdk_adapter


ALL_FORM_TOOLS = [
    "attachments",
    "internal_mcp",
    "pyodide",
    "data_analysis",
    "planner",
    "swarm",
    "lazy_tools_mode",
]


@pytest.fixture(autouse=True)
def _restore_cwd() -> Any:
    """`_prepare_sandbox_run_directory` calls `os.chdir` for real. It has to:
    there is no seam to fake a subprocess's inherited CWD without faking the
    whole subprocess. Restore the real CWD after every test in this file
    regardless of which test (or which fixture) changed it, so a chdir here
    never leaks into a test collected after this one.
    """

    original = os.getcwd()
    try:
        yield
    finally:
        os.chdir(original)


@pytest.fixture
def no_sandbox_backend(monkeypatch: pytest.MonkeyPatch) -> None:
    """The image this directory builds before #872: no Deno, no remote
    sandbox, no entrypoint."""

    monkeypatch.delenv("SANDBOX_SERVICE_URL", raising=False)
    monkeypatch.setattr(internal_tools.shutil, "which", lambda _name: None)


@pytest.fixture
def with_deno(monkeypatch: pytest.MonkeyPatch, tmp_path: Any) -> Any:
    """The image this directory builds as of #872: a `deno` on PATH, the
    baked entrypoint, and the private directories `ensure_sandbox_state_
    directories` would have created — SANDBOX_BASE's `tmp` subdirectory in
    particular, which `_prepare_sandbox_run_directory` needs to `chdir` into
    when a test keeps `pyodide` in its served set.
    """

    monkeypatch.delenv("SANDBOX_SERVICE_URL", raising=False)
    monkeypatch.setattr(internal_tools.shutil, "which", lambda name: f"/usr/bin/{name}")

    entrypoint = tmp_path / "main.ts"
    entrypoint.write_text("// stub entrypoint for tests\n")
    monkeypatch.setenv("PYODIDE_SANDBOX_PKG", str(entrypoint))

    sandbox_base = tmp_path / "sandbox-base"
    (sandbox_base / "tmp").mkdir(parents=True)
    monkeypatch.setenv("SANDBOX_BASE", str(sandbox_base))
    return sandbox_base


class TestSelection:
    def test_the_sandbox_tool_is_dropped_without_a_backend(
        self, no_sandbox_backend: None, capsys: pytest.CaptureFixture[str]
    ) -> None:
        served = internal_tools.serve_internal_tools(ALL_FORM_TOOLS)
        assert "pyodide" not in served

    def test_everything_else_still_reaches_the_sdk(
        self, no_sandbox_backend: None, capsys: pytest.CaptureFixture[str]
    ) -> None:
        # `planner` in particular. It is the one that failed FIRST in
        # production, and fixing it by dropping it would have turned a missing
        # directory into a missing feature.
        served = internal_tools.serve_internal_tools(ALL_FORM_TOOLS)
        assert served == [name for name in ALL_FORM_TOOLS if name != "pyodide"]

    def test_a_skip_is_reported_once_per_name_in_the_runtimes_shared_shape(
        self, no_sandbox_backend: None, capsys: pytest.CaptureFixture[str]
    ) -> None:
        # Same event name and fields as the Rust runtime's warning
        # (services/elitea-worker-rust/src/agents/internal_tools.rs), so one
        # grep covers a deployment running either worker.
        internal_tools.serve_internal_tools(["pyodide", "pyodide", "planner"])
        lines = [line for line in capsys.readouterr().err.splitlines() if line.strip()]
        assert [json.loads(line) for line in lines] == [
            {
                "event": "agent_internal_tool_skipped",
                "internal_tool": "pyodide",
                "reason_code": "sandbox_backend_unavailable",
            }
        ]

    def test_nothing_is_reported_when_nothing_is_dropped(
        self, with_deno: None, capsys: pytest.CaptureFixture[str]
    ) -> None:
        assert internal_tools.serve_internal_tools(ALL_FORM_TOOLS) == ALL_FORM_TOOLS
        assert capsys.readouterr().err == ""

    def test_a_remote_sandbox_service_serves_the_tool_too(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # The SDK takes a RemoteSandbox before it looks for Deno, so a
        # deployment that runs one must not have the tool pruned out from under
        # it.
        monkeypatch.setattr(internal_tools.shutil, "which", lambda _name: None)
        monkeypatch.setenv("SANDBOX_SERVICE_URL", "https://sandbox.internal")
        assert internal_tools.serve_internal_tools(["pyodide"]) == ["pyodide"]

    def test_a_blank_sandbox_url_is_not_a_backend(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.setattr(internal_tools.shutil, "which", lambda _name: None)
        monkeypatch.setenv("SANDBOX_SERVICE_URL", "   ")
        assert internal_tools.serve_internal_tools(["pyodide"]) == []

    def test_an_unknown_name_is_left_alone(self, no_sandbox_backend: None) -> None:
        # This module answers "can this IMAGE build it", not "is this a real
        # capability". The platform already refuses a name outside its own
        # catalogue (currentRuntimeInternalTools), and a second, narrower
        # allowlist here would silently drop the next tool the product adds.
        assert internal_tools.serve_internal_tools(["image_generation"]) == [
            "image_generation"
        ]

    def test_deno_alone_is_not_enough_without_an_entrypoint(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # #872: `deno` on PATH was the whole check before. `pkg_name` (from
        # PYODIDE_SANDBOX_PKG) is a SECOND precondition `BasePyodideSandbox`
        # raises on, and a deployment that has deno but no entrypoint must
        # still see the tool skipped, not a fresh RuntimeError deep in the SDK.
        monkeypatch.delenv("SANDBOX_SERVICE_URL", raising=False)
        monkeypatch.setattr(internal_tools.shutil, "which", lambda name: f"/usr/bin/{name}")
        monkeypatch.setenv("PYODIDE_SANDBOX_PKG", "/does/not/exist/main.ts")
        assert internal_tools.serve_internal_tools(["pyodide"]) == []

    def test_deno_and_an_existing_entrypoint_serve_the_tool(
        self, with_deno: Any
    ) -> None:
        assert internal_tools.serve_internal_tools(["pyodide"]) == ["pyodide"]

    def test_the_images_own_default_entrypoint_is_used_when_unset(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any
    ) -> None:
        # PYODIDE_SANDBOX_PKG unset (an operator did not name one) falls back
        # to where the Containerfile bakes the SDK's entrypoint, exactly as
        # the SDK's own get_default_pkg_name reads the environment variable
        # this module sets from the same default.
        default_entrypoint = tmp_path / "main.ts"
        default_entrypoint.write_text("// stub\n")
        monkeypatch.delenv("SANDBOX_SERVICE_URL", raising=False)
        monkeypatch.delenv("PYODIDE_SANDBOX_PKG", raising=False)
        monkeypatch.setattr(internal_tools.shutil, "which", lambda name: f"/usr/bin/{name}")
        monkeypatch.setattr(
            internal_tools, "_DEFAULT_PYODIDE_SANDBOX_PKG", str(default_entrypoint)
        )
        assert internal_tools._sandbox_backend_available() is True

    def test_a_remote_sandbox_service_needs_no_entrypoint(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # SANDBOX_SERVICE_URL takes a REMOTE sandbox — deno and the entrypoint
        # are this image's LOCAL execution path and irrelevant to it.
        monkeypatch.setattr(internal_tools.shutil, "which", lambda _name: None)
        monkeypatch.delenv("PYODIDE_SANDBOX_PKG", raising=False)
        monkeypatch.setenv("SANDBOX_SERVICE_URL", "https://sandbox.internal")
        assert internal_tools._sandbox_backend_available() is True


class TestStateDirectory:
    def test_an_operator_set_directory_is_never_relocated(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.setenv("ELITEA_DIR", "/somewhere/an/operator/chose")
        assert (
            internal_tools.ensure_sdk_state_directory()
            == "/somewhere/an/operator/chose"
        )
        assert os.environ["ELITEA_DIR"] == "/somewhere/an/operator/chose"

    def test_an_unset_directory_becomes_a_private_writable_one(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any
    ) -> None:
        # The defect: with ELITEA_DIR unset the SDK's FilesystemStorage falls
        # back to the RELATIVE path `.elitea/plans`, which in the worker
        # container resolves against `/` and cannot be created by uid 10001.
        monkeypatch.delenv("ELITEA_DIR", raising=False)
        monkeypatch.setattr(internal_tools.tempfile, "gettempdir", lambda: str(tmp_path))

        directory = internal_tools.ensure_sdk_state_directory()

        assert os.environ["ELITEA_DIR"] == directory
        assert os.path.isabs(directory)
        assert os.path.isdir(directory)
        # Plan files carry titles and step descriptions the model wrote, which
        # is conversation content.
        assert (os.stat(directory).st_mode & 0o777) == 0o700

    def test_it_is_idempotent(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any
    ) -> None:
        # Called on every run, beside `_apply_toolkit_guardrails`.
        monkeypatch.delenv("ELITEA_DIR", raising=False)
        monkeypatch.setattr(internal_tools.tempfile, "gettempdir", lambda: str(tmp_path))
        first = internal_tools.ensure_sdk_state_directory()
        second = internal_tools.ensure_sdk_state_directory()
        assert first == second


class TestSandboxStateDirectories:
    """`ensure_sandbox_state_directories` — the same class of fix as
    `ensure_sdk_state_directory`, applied to the SANDBOX's two cache
    directories, plus the entrypoint default.
    """

    def _clear(self, monkeypatch: pytest.MonkeyPatch) -> None:
        for name in ("SANDBOX_BASE", "DENO_DIR", "PYODIDE_SANDBOX_PKG"):
            monkeypatch.delenv(name, raising=False)

    def test_an_operator_set_sandbox_base_is_never_relocated(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any
    ) -> None:
        self._clear(monkeypatch)
        monkeypatch.setattr(internal_tools.tempfile, "gettempdir", lambda: str(tmp_path))
        monkeypatch.setenv("SANDBOX_BASE", "/somewhere/an/operator/chose")
        sandbox_base, _ = internal_tools.ensure_sandbox_state_directories()
        assert sandbox_base == "/somewhere/an/operator/chose"
        assert os.environ["SANDBOX_BASE"] == "/somewhere/an/operator/chose"

    def test_an_operator_set_deno_dir_is_never_relocated(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any
    ) -> None:
        self._clear(monkeypatch)
        monkeypatch.setattr(internal_tools.tempfile, "gettempdir", lambda: str(tmp_path))
        monkeypatch.setenv("DENO_DIR", "/opt/elitea/sandbox/deno-cache")
        _, deno_dir = internal_tools.ensure_sandbox_state_directories()
        assert deno_dir == "/opt/elitea/sandbox/deno-cache"
        assert os.environ["DENO_DIR"] == "/opt/elitea/sandbox/deno-cache"

    def test_unset_directories_become_private_and_writable(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any
    ) -> None:
        self._clear(monkeypatch)
        monkeypatch.setattr(internal_tools.tempfile, "gettempdir", lambda: str(tmp_path))

        sandbox_base, deno_dir = internal_tools.ensure_sandbox_state_directories()

        for directory in (sandbox_base, deno_dir):
            assert os.path.isabs(directory)
            assert os.path.isdir(directory)
            assert (os.stat(directory).st_mode & 0o777) == 0o700
        # SANDBOX_BASE's "tmp" subdirectory is NOT created here — see
        # TestSandboxRunDirectory, which creates it on demand instead.
        assert not os.path.isdir(os.path.join(sandbox_base, "tmp"))
        assert os.environ["SANDBOX_BASE"] == sandbox_base
        assert os.environ["DENO_DIR"] == deno_dir

    def test_an_operator_set_entrypoint_is_never_relocated(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any
    ) -> None:
        self._clear(monkeypatch)
        monkeypatch.setattr(internal_tools.tempfile, "gettempdir", lambda: str(tmp_path))
        monkeypatch.setenv("PYODIDE_SANDBOX_PKG", "/opt/elitea/sandbox/main.ts")
        internal_tools.ensure_sandbox_state_directories()
        assert os.environ["PYODIDE_SANDBOX_PKG"] == "/opt/elitea/sandbox/main.ts"

    def test_an_unset_entrypoint_gets_the_images_own_default(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any
    ) -> None:
        self._clear(monkeypatch)
        monkeypatch.setattr(internal_tools.tempfile, "gettempdir", lambda: str(tmp_path))
        internal_tools.ensure_sandbox_state_directories()
        assert os.environ["PYODIDE_SANDBOX_PKG"] == internal_tools._DEFAULT_PYODIDE_SANDBOX_PKG

    def test_it_is_idempotent(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any
    ) -> None:
        self._clear(monkeypatch)
        monkeypatch.setattr(internal_tools.tempfile, "gettempdir", lambda: str(tmp_path))
        first = internal_tools.ensure_sandbox_state_directories()
        second = internal_tools.ensure_sandbox_state_directories()
        assert first == second


class TestSandboxRunDirectory:
    """`_prepare_sandbox_run_directory` — deno materialises a `node_modules`
    tree under CWD on every run regardless of DENO_DIR's own cache, so the
    worker needs a writable CWD that is ALSO one of the three roots
    `_initialize_sandbox` grants read/write on.
    """

    def test_it_raises_without_ensure_sandbox_state_directories_first(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.delenv("SANDBOX_BASE", raising=False)
        with pytest.raises(RuntimeError):
            internal_tools._prepare_sandbox_run_directory()

    def test_it_chdirs_into_sandbox_bases_own_tmp_subdirectory(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any
    ) -> None:
        sandbox_base = tmp_path / "sandbox-base"
        (sandbox_base / "tmp").mkdir(parents=True)
        monkeypatch.setenv("SANDBOX_BASE", str(sandbox_base))

        internal_tools._prepare_sandbox_run_directory()

        # realpath: macOS resolves /tmp through a /private symlink, and a
        # bare os.getcwd() comparison would fail on this platform for a
        # correct implementation.
        assert os.path.realpath(os.getcwd()) == os.path.realpath(
            str(sandbox_base / "tmp")
        )

    def test_it_creates_tmp_even_under_an_operator_chosen_base(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any
    ) -> None:
        # `_initialize_sandbox` never creates SANDBOX_BASE's own "tmp"
        # subdirectory, operator-chosen base or not — the tool cannot run
        # without it either way, and `ensure_sandbox_state_directories`
        # deliberately does not touch an operator's SANDBOX_BASE at all, so
        # this is the one place that convention gets created.
        operator_base = tmp_path / "operator-chosen"
        operator_base.mkdir()
        monkeypatch.setenv("SANDBOX_BASE", str(operator_base))

        internal_tools._prepare_sandbox_run_directory()

        assert os.path.isdir(operator_base / "tmp")

    def test_serve_internal_tools_prepares_it_only_when_pyodide_survives(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Any, with_deno: Any
    ) -> None:
        # A turn that does not request (or cannot get) pyodide must not pay
        # for a CWD change it has no use for.
        original_cwd = os.getcwd()
        assert internal_tools.serve_internal_tools(["planner"]) == ["planner"]
        assert os.getcwd() == original_cwd

        internal_tools.serve_internal_tools(["pyodide"])
        assert os.path.realpath(os.getcwd()) == os.path.realpath(
            str(with_deno / "tmp")
        )


class TestVersionMetaPruning:
    """The stored-agent path prunes the VERSION META, not the payload field.

    `execute_application` never hands `payload.internal_tools` to the SDK. The
    SDK reads `version_details['meta']['internal_tools']` instead, and reads it
    twice — once to choose middleware, once to build tools — so the meta is the
    only place a prune can reach both.
    """

    def test_the_sandbox_tool_is_removed_from_the_frozen_meta(
        self, no_sandbox_backend: None
    ) -> None:
        version = {"meta": {"internal_tools": list(ALL_FORM_TOOLS), "step_limit": 7}}
        sdk_adapter._serve_version_internal_tools(version)
        assert version["meta"]["internal_tools"] == [
            name for name in ALL_FORM_TOOLS if name != "pyodide"
        ]
        assert version["meta"]["step_limit"] == 7

    @pytest.mark.parametrize(
        "version",
        [
            {},
            {"meta": None},
            {"meta": {}},
            {"meta": {"internal_tools": None}},
            {"meta": {"internal_tools": "pyodide"}},
            {"meta": {"internal_tools": ["pyodide", 7]}},
        ],
    )
    def test_a_shape_this_does_not_understand_is_left_untouched(
        self, no_sandbox_backend: None, version: dict[str, Any]
    ) -> None:
        # Removing names is this function's whole job. Repairing a malformed
        # value would change what the SDK sees for a reason that has nothing to
        # do with what this image can build.
        original = json.loads(json.dumps(version))
        sdk_adapter._serve_version_internal_tools(version)
        assert version == original


class TestSandboxRealExecution:
    """One real, non-mocked run through the sandbox path.

    Everything above this class fakes `deno` and the entrypoint to test this
    module's OWN bookkeeping in isolation. That leaves the thing #872 is
    actually about unverified: does the SDK's `PyodideSandboxTool` execute
    code when this module says it can? MEASURED, in the image this directory
    builds (see the Containerfile) and in the CI job that pre-warms the same
    entrypoint at the same default path before this file runs (see
    ci-python.yml) — both give this test a real `deno` and a real, already
    network-warmed entrypoint, and it is skipped everywhere else (a bare `pip
    install` of this directory's extras carries neither).
    """

    @pytest.fixture(autouse=True)
    def _require_a_real_sandbox_backend(self) -> None:
        if shutil.which("deno") is None:
            pytest.skip("deno is not on PATH in this environment")
        entrypoint = internal_tools._pyodide_entrypoint_path()
        if not os.path.isfile(entrypoint):
            pytest.skip(f"no Pyodide sandbox entrypoint at {entrypoint!r}")

    def test_a_trivial_snippet_executes_through_the_real_sdk_sandbox(
        self, tmp_path: Any, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # SANDBOX_BASE is reset to a fresh directory so this test does not
        # depend on (or leave behind) real state from a previous run.
        # DENO_DIR and PYODIDE_SANDBOX_PKG are left exactly as the real
        # environment already has them: both point at an already-warmed cache
        # in CI and in the shipped image, and re-defaulting DENO_DIR here
        # would point at an empty one this test would then have to fill over
        # the network.
        monkeypatch.delenv("SANDBOX_BASE", raising=False)
        monkeypatch.setattr(internal_tools.tempfile, "gettempdir", lambda: str(tmp_path))

        internal_tools.ensure_sandbox_state_directories()
        assert internal_tools.serve_internal_tools(["pyodide"]) == ["pyodide"]

        from elitea_sdk.runtime.tools.sandbox import SandboxToolkit

        toolkit = SandboxToolkit.get_toolkit(stateful=False, allow_net=True)
        tools = {tool.name: tool for tool in toolkit.get_tools()}
        result = tools["pyodide_sandbox"]._run(
            "print('hello from the sandbox')\nresult = 21 * 2\nresult"
        )

        assert "error" not in result, result
        assert result.get("output") == "hello from the sandbox"
        assert result.get("result") == 42
