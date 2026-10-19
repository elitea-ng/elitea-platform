"""Contract gate between the elitea-sdk budget reader and the gateway refusal.

WHY THIS FILE EXISTS
--------------------
The gateway wrote a 402 that looked correct. The SCOPE of the refusal was in
error.type. Every Go test agreed with every other Go test, because all of them
restated the same wrong literal. elitea-sdk reads that body in one function,
``elitea_sdk/runtime/exceptions.py::budget_exceeded_from``. That function
matches on error.type ALONE, and it reads the scope from error.code. It thus
returned None. No typed exception came out. The policy refusal went to the
model as message content. Nothing failed.

This gate reads BOTH sides from source and compares them:

  * the SDK side comes from the pinned SDK tree at ELITEA_SDK_SOURCE_ROOT;
  * the gateway side comes from the Go source in this repository.

No literal from either contract is written into the assertions. A test that
restates a literal only proves that two copies of one mistake agree.

The SDK reader also runs for real here. The gate builds the body that the
gateway writes, gives it to the SDK function, and looks at the scope that comes
back. A structural check alone cannot see the defect above, because the
structure was correct: only the two values were in the wrong fields.

THE PINNED TREE IS CHECKED BY CONTENT, NOT ONLY BY REVISION
-----------------------------------------------------------
``git rev-parse HEAD`` answers which commit is checked out. It does not answer
what the working tree holds. A dirty checkout keeps the pinned revision, and
``git rev-parse`` walks up to an enclosing repository, so a hand-authored
directory inside a pinned clone reports the pinned revision too. This gate
therefore hashes every SDK file it reads and compares the digest with
``internal/sdkpin/sdk-pin.json`` (#567). The check is scoped to those files:
CI applies two cherry-picks with ``--no-commit``, so the CI tree is dirty on
purpose and a whole-tree cleanliness check would fail every run.

WHY THIS GATE MUST NOT SKIP QUIETLY
-----------------------------------
An absent input is the failure mode that this repository keeps finding: the
check reports a pass because it looked at nothing. When the SDK source is not
available, this gate FAILS under CI. It skips only for a local developer, and
the skip message names the variable to set.
"""

from __future__ import annotations

import ast
from collections import Counter
import hashlib
import json
import os
import re
import subprocess
from pathlib import Path
from typing import Dict, List, Set, Tuple

import pytest

# ---------------------------------------------------------------------------
# Locations
# ---------------------------------------------------------------------------

REPO_ROOT = Path(__file__).resolve().parents[2]
GATEWAY_ROOT = REPO_ROOT / "services" / "elitea-llm-gateway"
BUDGET_GATE_GO = GATEWAY_ROOT / "internal" / "llmproxy" / "budget_gate.go"
ROUTER_GO = GATEWAY_ROOT / "internal" / "api" / "router.go"

SDK_EXCEPTIONS_PY = Path("elitea_sdk") / "runtime" / "exceptions.py"
SDK_CLIENT_PY = Path("elitea_sdk") / "runtime" / "clients" / "client.py"

# Every SDK file this gate reads. The pin must state the content of each one,
# and the test below compares the two lists. A gate that starts reading a third
# file without pinning it measures bytes nobody recorded.
GATE_SDK_FILES = (SDK_EXCEPTIONS_PY, SDK_CLIENT_PY)

SDK_ROOT_ENV = "ELITEA_SDK_SOURCE_ROOT"

# The gateway's own statement about which elitea-sdk revision its compatibility
# gates were verified against. It is read here rather than restated, so this
# file and services/elitea-llm-gateway/scripts/sdk-conformance/run.sh cannot
# drift into measuring two different revisions.
#
# The revision itself is OWNED by services/elitea-llm-gateway/
# internal/sdkpin/elitea-sdk.lock.json; the pin below records what the gateway was verified
# against, and internal/sdkpin/pin_test.go fails when the two disagree.
GATEWAY_SDK_PIN = GATEWAY_ROOT / "internal" / "sdkpin" / "sdk-pin.json"

_FULL_GIT_REVISION = re.compile(r"^[0-9a-f]{40}$")
_SHA256_DIGEST = re.compile(r"^[0-9a-f]{64}$")

# The two suffixes below are NOT in the SDK source. LangChain adds them to the
# base_url that the SDK gives it. They are listed here with the class that adds
# each one. The test that uses them first proves, from the SDK AST, that the
# class still gets that base_url. If the SDK moves ChatAnthropic onto llm_path,
# the binding check fails before these suffixes are used.
LIBRARY_SUFFIXES: Dict[str, Tuple[str, ...]] = {
    # ChatOpenAI posts to {base_url}/chat/completions.
    # OpenAIEmbeddings posts to {base_url}/embeddings.
    "llm_path": ("/chat/completions", "/embeddings"),
    # ChatAnthropic posts to {base_url}/v1/messages.
    "allm_path": ("/v1/messages",),
}


# ---------------------------------------------------------------------------
# Input handling: fail under CI, skip only for a local developer
# ---------------------------------------------------------------------------


def _running_under_ci() -> bool:
    """Tell if this runs in CI. GitHub Actions sets CI=true."""
    return os.environ.get("CI", "").strip().lower() not in ("", "0", "false", "no")


def _no_sdk(reason: str) -> None:
    """Stop the run when the SDK source is not usable.

    WHY: a silent skip here would let a broken contract merge. That is the
    exact defect class this gate exists for. Under CI there is no legitimate
    reason for the source to be absent, so the run FAILS.
    """
    message = (
        "{reason}\n"
        "This gate compares the pinned elitea-sdk source with the gateway Go "
        "source. Set {env} to a checkout of the SDK revision that "
        ".github/workflows/ci-python.yml pins, then run this test again."
    ).format(reason=reason, env=SDK_ROOT_ENV)
    if _running_under_ci():
        pytest.fail(
            "CI=1 and the SDK source is not available. A skip here would "
            "report a pass for a comparison that never happened.\n" + message,
            pytrace=False,
        )
    pytest.skip(message)


def _load_pin() -> Dict[str, object]:
    """Read the gateway SDK pin.

    A missing or unparsable pin FAILS. It is a file in this checkout, so it is
    never legitimately absent, and an absent pin would leave every comparison
    below with nothing on the other side.
    """
    if not GATEWAY_SDK_PIN.is_file():
        pytest.fail(
            "the gateway SDK pin {path!s} is missing. This gate must know WHICH "
            "elitea-sdk revision it is allowed to measure; without it a green "
            "result says nothing about the revision the platform ships.".format(
                path=GATEWAY_SDK_PIN
            ),
            pytrace=False,
        )
    try:
        return json.loads(GATEWAY_SDK_PIN.read_text(encoding="utf-8"))
    except ValueError as error:
        pytest.fail(
            "{path!s} does not parse as JSON: {error}".format(
                path=GATEWAY_SDK_PIN, error=error
            ),
            pytrace=False,
        )


def _pinned_revision() -> str:
    """Read the SDK revision the gateway declares it was verified against.

    A malformed revision FAILS: an empty value would make the revision
    comparison below compare against nothing.
    """
    pin = _load_pin()
    revision = str((pin.get("verified_against") or {}).get("revision") or "")
    if not _FULL_GIT_REVISION.match(revision):
        pytest.fail(
            "{path!s} carries verified_against.revision {rev!r}, which is not a "
            "40-character git object name. A short or empty value cannot be "
            "compared with what `git rev-parse HEAD` reports.".format(
                path=GATEWAY_SDK_PIN, rev=revision
            ),
            pytrace=False,
        )
    return revision


def _pinned_contents() -> Dict[str, str]:
    """Give the sha256 the pin records for each SDK file the gates read.

    WHY CONTENT, AND NOT ONLY A REVISION (#567). This gate used to identify the
    SDK checkout by ``git rev-parse HEAD`` alone. That answers WHICH commit is
    checked out, not WHAT the working tree holds. A DIRTY tree keeps the pinned
    HEAD, so uncommitted SDK edits were invisible here; and ``git rev-parse``
    walks UP to an enclosing repository, so a hand-authored directory inside a
    pinned clone reported the pinned revision. Either one lets an operator mint
    a fresh pin from a green run that measured other bytes.

    An absent, empty or malformed list FAILS. A gate with nothing to hash
    passes for every tree, which is the hole this list closes.
    """
    pin = _load_pin()
    entries = (pin.get("verified_against") or {}).get("contents")
    if not isinstance(entries, list) or not entries:
        pytest.fail(
            "{path!s} carries no verified_against.contents. This gate must "
            "compare the CONTENT of the SDK files it reads, not only the "
            "revision: a dirty checkout keeps the pinned revision and would "
            "read as pinned.".format(path=GATEWAY_SDK_PIN),
            pytrace=False,
        )
    digests: Dict[str, str] = {}
    for entry in entries:
        path = str((entry or {}).get("path") or "")
        digest = str((entry or {}).get("sha256") or "")
        if not path or not _SHA256_DIGEST.match(digest):
            pytest.fail(
                "{path!s} carries the contents entry {entry!r}, which is not a "
                "path with a 64-character lowercase sha256. A digest that "
                "matches nothing any hashing tool prints fails for the wrong "
                "reason.".format(path=GATEWAY_SDK_PIN, entry=entry),
                pytrace=False,
            )
        if path in digests:
            pytest.fail(
                "{path!s} lists {name} twice; two digests for one file cannot "
                "both hold.".format(path=GATEWAY_SDK_PIN, name=path),
                pytrace=False,
            )
        digests[path] = digest
    return digests


def _assert_checkout_content(root: Path) -> None:
    """Hash every pinned file in the checkout and compare it with the pin.

    This is the check the revision comparison cannot make. It ALWAYS fails on a
    difference, under CI or not: a green result about bytes the pin does not
    name is worse than no result.

    It is SCOPED to the pinned files on purpose. ``.github/workflows/
    ci-python.yml`` applies the two patch revisions with ``git cherry-pick
    --no-commit``, so the CI tree is deliberately dirty. A whole-tree
    cleanliness check would fail every CI run; those patches touch
    ``toolkits/mcp.py``, ``utils/mcp_adapter.py`` and ``tests/`` only, and none
    of them is a file this gate reads.
    """
    for relative, expected in sorted(_pinned_contents().items()):
        path = root / Path(relative)
        if not path.is_file():
            pytest.fail(
                "{pin!s} pins the content of {rel}, and {path!s} is not there. "
                "The gate would then measure a tree the pin does not "
                "describe.".format(pin=GATEWAY_SDK_PIN, rel=relative, path=path),
                pytrace=False,
            )
        actual = hashlib.sha256(path.read_bytes()).hexdigest()
        if actual != expected:
            pytest.fail(
                "the SDK checkout holds the pinned revision, and {rel} does NOT "
                "hold the pinned content.\n"
                "  {path!s}\n"
                "    is       sha256 {actual}\n"
                "    pinned as sha256 {expected}\n"
                "A revision names which commit is checked out. It says nothing "
                "about what the working tree holds, so an edited or "
                "hand-authored file keeps the pinned revision and would "
                "otherwise read as pinned. Restore the file, or — if the SDK "
                "moved on purpose — re-run both compatibility gates and update "
                "the pin AFTER they pass.".format(
                    rel=relative, path=path, actual=actual, expected=expected
                ),
                pytrace=False,
            )


def _checkout_revision(root: Path) -> str:
    """Report the revision of the SDK checkout, or "" when git cannot say."""
    try:
        completed = subprocess.run(  # noqa: S603
            ["git", "-C", str(root), "rev-parse", "HEAD"],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return ""
    if completed.returncode != 0:
        return ""
    return completed.stdout.strip()


@pytest.fixture(scope="session")
def sdk_root() -> Path:
    """Give the root of the pinned SDK source tree.

    The tree must BE the pinned revision. Comparing the gateway against some
    other checkout produces a green result about a revision the platform does
    not ship, which is worse than no result at all.
    """
    raw = os.environ.get(SDK_ROOT_ENV, "").strip()
    if not raw:
        _no_sdk("{env} is not set.".format(env=SDK_ROOT_ENV))
    root = Path(raw)
    for relative in (SDK_EXCEPTIONS_PY, SDK_CLIENT_PY):
        if not (root / relative).is_file():
            _no_sdk(
                "{env} is {root!s}, but {rel!s} is not there.".format(
                    env=SDK_ROOT_ENV, root=root, rel=relative
                )
            )

    pinned = _pinned_revision()
    actual = _checkout_revision(root)
    if not actual:
        # An unidentifiable tree is not a pinned tree. Under CI that is a
        # broken workflow step, and a skip there would report a pass for a
        # comparison against an unknown revision.
        _no_sdk(
            "cannot read the git revision of {root!s}; this gate must know which "
            "SDK revision it measured.".format(root=root)
        )
    if actual != pinned:
        # ALWAYS a failure, CI or not. The workflow applies the pinned
        # cherry-picks with `git cherry-pick --no-commit`, which leaves HEAD at
        # the base revision, so this comparison holds there too.
        pytest.fail(
            "the SDK checkout is not the pinned revision.\n"
            "  {env} = {root!s} is at {actual}\n"
            "  {pin!s} names {pinned}\n"
            "Point {env} at the pinned revision. If the SDK moved on purpose, "
            "update services/elitea-llm-gateway/internal/sdkpin/elitea-sdk.lock.json, re-run "
            "this gate and the tier 2 conformance script against the new "
            "revision, and only then update the gateway pin.".format(
                env=SDK_ROOT_ENV,
                root=root,
                actual=actual,
                pin=GATEWAY_SDK_PIN,
                pinned=pinned,
            ),
            pytrace=False,
        )

    # The revision says WHICH commit. This says WHAT the tree holds (#567).
    _assert_checkout_content(root)
    return root


def _read_gateway_source(path: Path) -> str:
    """Read a Go file of this repository.

    The gateway source is part of this checkout. It is never optional, so a
    missing file FAILS for a local developer too.
    """
    if not path.is_file():
        pytest.fail(
            "gateway source {path!s} is missing. This gate cannot compare a "
            "contract that it cannot read.".format(path=path),
            pytrace=False,
        )
    return path.read_text(encoding="utf-8")


# ---------------------------------------------------------------------------
# Go source parsing
# ---------------------------------------------------------------------------


def _strip_go_comments(src: str) -> str:
    """Remove Go comments, but keep the string literals.

    WHY: budget_gate.go explains the wire contract in prose, and that prose
    repeats every literal that it defines. "insufficient_quota" is in a comment
    two lines above the constant of the same value. A regular expression over
    the raw text can match the comment and report a pass after somebody changed
    the constant itself. router.go has the same property: its package comment
    contains the route paths.

    Each comment becomes one space, so that tokens do not join.
    """
    out: List[str] = []
    i = 0
    n = len(src)
    while i < n:
        c = src[i]
        if c == "/" and i + 1 < n and src[i + 1] == "/":
            while i < n and src[i] != "\n":
                i += 1
            out.append(" ")
            continue
        if c == "/" and i + 1 < n and src[i + 1] == "*":
            i += 2
            while i + 1 < n and not (src[i] == "*" and src[i + 1] == "/"):
                i += 1
            i += 2
            out.append(" ")
            continue
        if c in ('"', "'"):
            quote = c
            out.append(c)
            i += 1
            while i < n:
                out.append(src[i])
                if src[i] == "\\":
                    if i + 1 < n:
                        out.append(src[i + 1])
                        i += 2
                        continue
                if src[i] == quote:
                    i += 1
                    break
                i += 1
            continue
        if c == "`":
            out.append(c)
            i += 1
            while i < n and src[i] != "`":
                out.append(src[i])
                i += 1
            if i < n:
                out.append("`")
                i += 1
            continue
        out.append(c)
        i += 1
    return "".join(out)


def _go_string_const(src: str, name: str) -> str:
    """Read one Go string constant by name from comment-free source."""
    pattern = re.compile(
        r"\b" + re.escape(name) + r'\s*=\s*"((?:[^"\\]|\\.)*)"'
    )
    values = pattern.findall(src)
    if not values:
        pytest.fail(
            "the Go constant {name} is not in {path!s} (outside comments). "
            "Either it was renamed or its value is no longer a plain string "
            "literal. This gate reads the contract from that constant, so it "
            "cannot continue.".format(name=name, path=BUDGET_GATE_GO),
            pytrace=False,
        )
    unique = set(values)
    if len(unique) != 1:
        pytest.fail(
            "the Go constant {name} has more than one value in {path!s}: "
            "{values!r}".format(name=name, path=BUDGET_GATE_GO, values=sorted(unique)),
            pytrace=False,
        )
    value = values[0]
    if "\\" in value:
        pytest.fail(
            "the Go constant {name} now uses an escape sequence ({value!r}). "
            "Extend this parser before you change the constant.".format(
                name=name, value=value
            ),
            pytrace=False,
        )
    return value


class GatewayBudgetContract:
    """The three wire values that every gateway budget refusal carries."""

    def __init__(self, error_type: str, project_code: str, member_code: str):
        self.error_type = error_type
        self.project_code = project_code
        self.member_code = member_code

    @property
    def codes(self) -> Set[str]:
        return {self.project_code, self.member_code}


@pytest.fixture(scope="session")
def gateway() -> GatewayBudgetContract:
    """Read the budget refusal contract out of the gateway Go source."""
    src = _strip_go_comments(_read_gateway_source(BUDGET_GATE_GO))
    contract = GatewayBudgetContract(
        error_type=_go_string_const(src, "budgetErrorType"),
        project_code=_go_string_const(src, "budgetCodeProject"),
        member_code=_go_string_const(src, "budgetCodeMember"),
    )
    if contract.project_code == contract.member_code:
        pytest.fail(
            "the gateway project code and member code are both {code!r}. The "
            "SDK reads the scope from this value, so the two ceilings would be "
            "indistinguishable to every caller.".format(code=contract.project_code),
            pytrace=False,
        )
    return contract


_GO_ROUTE_RE = re.compile(r"\br\.Route\(\s*\"([^\"]*)\"")
_GO_METHOD_RE = re.compile(
    r"\br\.(?:Get|Post|Put|Patch|Delete|Head|Options|Handle|HandleFunc)"
    r"\(\s*\"([^\"]*)\""
)


@pytest.fixture(scope="session")
def mounted_routes() -> Set[str]:
    """Give every absolute path that internal/api/router.go mounts.

    The paths are parsed from the router source. A hard-coded list would keep
    passing after somebody moved a route.
    """
    src = _strip_go_comments(_read_gateway_source(ROUTER_GO))
    prefixes = _GO_ROUTE_RE.findall(src)
    if len(prefixes) != 1:
        pytest.fail(
            "{path!s} registers {count} r.Route(...) groups. This parser "
            "handles exactly one, so with more it would report the wrong set "
            "of paths. Extend the parser.".format(path=ROUTER_GO, count=len(prefixes)),
            pytrace=False,
        )
    prefix = prefixes[0].rstrip("/")
    suffixes = _GO_METHOD_RE.findall(src)
    if not suffixes:
        pytest.fail(
            "no route is registered in {path!s}. An empty route set would let "
            "every path check below pass for nothing.".format(path=ROUTER_GO),
            pytrace=False,
        )
    return {prefix + suffix for suffix in suffixes}


def _router_serves(mounted: Set[str], path: str) -> bool:
    """Tell if the router answers this exact path.

    A chi route that ends with "*" answers every path below it.
    """
    for route in mounted:
        if route.endswith("/*"):
            if path.startswith(route[:-1]):
                return True
        elif route == path:
            return True
    return False


# ---------------------------------------------------------------------------
# SDK source parsing
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session")
def sdk_exceptions(sdk_root: Path) -> Dict[str, object]:
    """Run elitea_sdk/runtime/exceptions.py in an isolated namespace.

    WHY not "import elitea_sdk.runtime.exceptions": the installed package can
    be a different revision from the pinned tree that this gate points at, and
    the package __init__ pulls in the whole LangChain runtime. This module has
    no imports of its own, so the source runs on its own.

    The namespace holds the real BUDGET_ERROR_TYPE, BUDGET_SCOPES,
    DEFAULT_BUDGET_SCOPE and budget_exceeded_from. The tests below call that
    function for real.
    """
    path = sdk_root / SDK_EXCEPTIONS_PY
    namespace: Dict[str, object] = {
        "__name__": "elitea_sdk_runtime_exceptions_contract_probe",
        "__file__": str(path),
    }
    source = path.read_text(encoding="utf-8")
    exec(compile(source, str(path), "exec"), namespace)  # noqa: S102
    for symbol in (
        "BUDGET_ERROR_TYPE",
        "BUDGET_SCOPES",
        "DEFAULT_BUDGET_SCOPE",
        "BudgetExceededError",
        "budget_exceeded_from",
    ):
        if symbol not in namespace:
            pytest.fail(
                "the pinned SDK {path!s} no longer defines {symbol}. The "
                "gateway contract is built around it.".format(
                    path=path, symbol=symbol
                ),
                pytrace=False,
            )
    scopes = namespace["BUDGET_SCOPES"]
    if not isinstance(scopes, tuple) or not scopes:
        pytest.fail(
            "BUDGET_SCOPES is {scopes!r}. An empty or non-tuple value would "
            "make every scope check below vacuous.".format(scopes=scopes),
            pytrace=False,
        )
    return namespace


@pytest.fixture(scope="session")
def sdk_exceptions_ast(sdk_root: Path) -> ast.Module:
    path = sdk_root / SDK_EXCEPTIONS_PY
    return ast.parse(path.read_text(encoding="utf-8"), filename=str(path))


@pytest.fixture(scope="session")
def sdk_client_ast(sdk_root: Path) -> ast.Module:
    path = sdk_root / SDK_CLIENT_PY
    return ast.parse(path.read_text(encoding="utf-8"), filename=str(path))


class FakeUpstreamError(Exception):
    """Stand in for openai.APIError / anthropic.APIStatusError.

    budget_exceeded_from reads exactly one attribute off the exception: .body.

    The message text holds none of the contract strings on purpose. The SDK has
    a second, text-based path for callers that lose the structured body. If the
    message carried the contract strings, that path could answer for a body
    that the structured path rejects, and a negative test would pass for the
    wrong reason.
    """

    def __init__(self, body: object):
        super().__init__("upstream call failed")
        self.body = body


def _openai_shape(error_type: str, code: object) -> Dict[str, object]:
    """Build the body as the OpenAI client stores it: the wrapper is removed."""
    body: Dict[str, object] = {
        "type": error_type,
        "message": "budget exhausted for this billing period",
        "param": None,
    }
    if code is not None:
        body["code"] = code
    return body


def _anthropic_shape(error_type: str, code: object) -> Dict[str, object]:
    """Build the body as the Anthropic client stores it: the wrapper stays."""
    return {"type": "error", "error": _openai_shape(error_type, code)}


BODY_SHAPES = {"openai": _openai_shape, "anthropic": _anthropic_shape}


# ---------------------------------------------------------------------------
# 1. The error type
# ---------------------------------------------------------------------------


def test_sdk_error_type_equals_the_gateway_error_type(
    sdk_exceptions: Dict[str, object], gateway: GatewayBudgetContract
) -> None:
    """The SDK match key and the gateway wire type must be the same string.

    Prevents: the gateway writes a type that the SDK does not match, so
    budget_exceeded_from returns None and the refusal reaches the model as
    message content.
    """
    assert sdk_exceptions["BUDGET_ERROR_TYPE"] == gateway.error_type


# ---------------------------------------------------------------------------
# 2. Every SDK scope is reachable, and the member code is one of them
# ---------------------------------------------------------------------------


def test_gateway_codes_produce_exactly_the_sdk_scopes(
    sdk_exceptions: Dict[str, object], gateway: GatewayBudgetContract
) -> None:
    """Each gateway code must give a scope, and together they must give all.

    The set is compared in both directions on purpose:

      * a gateway code that gives no matching scope means a refusal that the
        platform labels with the wrong ceiling;
      * an SDK scope that no gateway code can produce means a scope that the
        agent can never see, so the front end message behind it is dead.

    Prevents: a rename on either side. The member code is the fragile one,
    because the SDK needs that exact spelling to report the member scope.
    """
    budget_exceeded_from = sdk_exceptions["budget_exceeded_from"]
    scopes = set(sdk_exceptions["BUDGET_SCOPES"])

    assert gateway.member_code in scopes, (
        "the gateway member code {code!r} is not an SDK budget scope. The SDK "
        "would resolve it to the default (project) scope, and every member cap "
        "refusal would be reported as a project cap.".format(
            code=gateway.member_code
        )
    )

    produced = {}
    for code in sorted(gateway.codes):
        for shape_name, shape in sorted(BODY_SHAPES.items()):
            error = budget_exceeded_from(
                FakeUpstreamError(shape(gateway.error_type, code))
            )
            assert error is not None, (
                "the SDK does not recognise the gateway refusal "
                "{code!r} in the {shape} body shape.".format(
                    code=code, shape=shape_name
                )
            )
            produced.setdefault(code, set()).add(error.scope)

    for code, got in produced.items():
        assert len(got) == 1, (
            "the gateway code {code!r} gives different scopes for the OpenAI "
            "and the Anthropic body shape: {got!r}".format(code=code, got=got)
        )

    got_scopes = {next(iter(v)) for v in produced.values()}
    assert got_scopes == scopes, (
        "the gateway codes {codes!r} produce the scopes {got!r}, but the SDK "
        "declares {want!r}.".format(
            codes=sorted(gateway.codes), got=sorted(got_scopes), want=sorted(scopes)
        )
    )


# ---------------------------------------------------------------------------
# 3. The project code depends on the SDK default
# ---------------------------------------------------------------------------


def test_project_code_relies_on_the_sdk_default_scope(
    sdk_exceptions: Dict[str, object], gateway: GatewayBudgetContract
) -> None:
    """The project code is deliberately not an SDK scope. Pin what carries it.

    The gateway keeps the OpenAI canonical code for the project ceiling, so a
    generic OpenAI client understands the refusal. The SDK never lists that
    code. It works only because the SDK resolves an unknown code to
    DEFAULT_BUDGET_SCOPE, and that default is the project scope.

    That is a RELIANCE, not a coincidence. This test states it, so an SDK that
    stops defaulting, or that moves the default onto the member scope, fails
    here instead of mislabelling every project cap refusal in production.
    """
    budget_exceeded_from = sdk_exceptions["budget_exceeded_from"]
    scopes = set(sdk_exceptions["BUDGET_SCOPES"])
    default_scope = sdk_exceptions["DEFAULT_BUDGET_SCOPE"]

    assert gateway.project_code not in scopes, (
        "the gateway project code {code!r} is now an SDK scope. The reliance "
        "below is no longer needed, and this test must be rewritten to assert "
        "the direct match instead.".format(code=gateway.project_code)
    )

    # The default must be the scope that is NOT the member one. This is derived
    # from both sources: the gateway member code names the member scope, so
    # what is left in BUDGET_SCOPES is the project scope.
    assert scopes - {gateway.member_code} == {default_scope}, (
        "DEFAULT_BUDGET_SCOPE is {default!r}, but the remaining scope in "
        "{scopes!r} after the gateway member code {member!r} is not that "
        "value. The gateway project refusal would be labelled with the wrong "
        "ceiling.".format(
            default=default_scope, scopes=sorted(scopes), member=gateway.member_code
        )
    )

    for shape_name, shape in sorted(BODY_SHAPES.items()):
        error = budget_exceeded_from(
            FakeUpstreamError(shape(gateway.error_type, gateway.project_code))
        )
        assert error is not None, (
            "the SDK does not recognise the project refusal in the {shape} "
            "body shape.".format(shape=shape_name)
        )
        assert error.scope == default_scope

        # The reliance itself: any code the SDK does not know must fall back to
        # the default. The gateway project code depends on this branch.
        unknown = budget_exceeded_from(
            FakeUpstreamError(
                shape(gateway.error_type, "a-code-no-sdk-release-declares")
            )
        )
        assert unknown is not None, (
            "the SDK now returns None for an unrecognised code in the {shape} "
            "body shape. The gateway project refusal uses an unrecognised code "
            "on purpose, so it is no longer recognised at all.".format(
                shape=shape_name
            )
        )
        assert unknown.scope == default_scope, (
            "the SDK no longer resolves an unrecognised code to "
            "DEFAULT_BUDGET_SCOPE. The gateway project refusal depends on it."
        )


# ---------------------------------------------------------------------------
# 4. The SDK paths against the routes that the gateway mounts
# ---------------------------------------------------------------------------


def _joined_str_parts(node: ast.JoinedStr) -> Tuple[List[str], str]:
    """Split an f-string into its interpolated names and its trailing text."""
    attributes: List[str] = []
    tail = ""
    for value in node.values:
        if isinstance(value, ast.FormattedValue):
            inner = value.value
            if isinstance(inner, ast.Attribute):
                attributes.append(inner.attr)
            else:
                attributes.append(ast.dump(inner))
            tail = ""
        elif isinstance(value, ast.Constant) and isinstance(value.value, str):
            tail += value.value
    return attributes, tail


# The two attributes that hold the base path of every LLM call the SDK makes.
SDK_PATH_ATTRIBUTES = ("llm_path", "allm_path")


def _self_attr_constants(node: ast.AST) -> Dict[str, str]:
    """Collect the plain "self.<name> = '<literal>'" values below a node."""
    found: Dict[str, str] = {}
    for child in ast.walk(node):
        if not isinstance(child, ast.Assign):
            continue
        if not isinstance(child.value, ast.Constant):
            continue
        if not isinstance(child.value.value, str):
            continue
        for target in child.targets:
            if (
                isinstance(target, ast.Attribute)
                and isinstance(target.value, ast.Name)
                and target.value.id == "self"
            ):
                found[target.attr] = child.value.value
    return found


def _sdk_base_paths(tree: ast.Module) -> Dict[str, str]:
    """Read the SDK client base paths from the class that defines them.

    The class is found by what it assigns, not by its name. A rename of the
    class is not a contract change; a change of the paths is.
    """
    matches: Dict[str, Dict[str, str]] = {}
    for node in ast.walk(tree):
        if not isinstance(node, ast.ClassDef):
            continue
        constants = _self_attr_constants(node)
        if all(name in constants for name in SDK_PATH_ATTRIBUTES):
            matches[node.name] = constants
    if len(matches) != 1:
        pytest.fail(
            "expected exactly one SDK class that assigns literal "
            "{names} to self; found {found!r}. Either the client was "
            "restructured or the paths are now computed, and this gate can no "
            "longer read them.".format(
                names=" and ".join(SDK_PATH_ATTRIBUTES), found=sorted(matches)
            ),
            pytrace=False,
        )
    constants = next(iter(matches.values()))
    # Only the two LLM base paths. The same class holds many other literal
    # paths (the /api/v2 surfaces), and those are not served by this router.
    return {name: constants[name] for name in SDK_PATH_ATTRIBUTES}


def _as_block(body: List[ast.stmt]) -> ast.Module:
    """Wrap one branch body so that ast.walk() cannot reach its sibling."""
    return ast.Module(body=list(body), type_ignores=[])


def _base_url_attribute(node: ast.AST) -> str:
    """Find which self.*_path feeds the one base_url below this node."""
    attributes: Set[str] = set()
    for child in ast.walk(node):
        f_string = None
        if isinstance(child, ast.keyword) and child.arg == "base_url":
            if isinstance(child.value, ast.JoinedStr):
                f_string = child.value
        elif isinstance(child, ast.Dict):
            for key, value in zip(child.keys, child.values):
                if (
                    isinstance(key, ast.Constant)
                    and key.value == "base_url"
                    and isinstance(value, ast.JoinedStr)
                ):
                    f_string = value
        if f_string is None:
            continue
        names, tail = _joined_str_parts(f_string)
        if tail:
            pytest.fail(
                "an SDK base_url f-string now ends with the literal {tail!r}. "
                "This gate assumes the base_url is the bare path.".format(tail=tail),
                pytrace=False,
            )
        if names:
            attributes.add(names[-1])
    if len(attributes) != 1:
        pytest.fail(
            "expected exactly one base_url binding here, found {found!r}. The "
            "SDK client was restructured; extend this gate before you trust "
            "it.".format(found=sorted(attributes)),
            pytrace=False,
        )
    return attributes.pop()


def _function(tree: ast.Module, name: str) -> ast.FunctionDef:
    for node in ast.walk(tree):
        if isinstance(node, ast.FunctionDef) and node.name == name:
            return node
    pytest.fail(
        "the pinned SDK no longer defines {name}(). This gate reads the "
        "contract out of it.".format(name=name),
        pytrace=False,
    )
    raise AssertionError  # unreachable; keeps the type checkers quiet


def _anthropic_branch(get_llm: ast.FunctionDef) -> ast.If:
    for node in ast.walk(get_llm):
        if isinstance(node, ast.If) and isinstance(node.test, ast.Name):
            if node.test.id == "is_anthropic":
                return node
    pytest.fail(
        "get_llm() no longer branches on is_anthropic. This gate reads the "
        "ChatAnthropic base_url out of that branch.",
        pytrace=False,
    )
    raise AssertionError  # unreachable


def test_sdk_llm_paths_are_mounted_by_the_gateway_router(
    sdk_client_ast: ast.Module, mounted_routes: Set[str]
) -> None:
    """Every endpoint the SDK calls must be a route the gateway mounts.

    The SDK holds two base paths, and the LangChain class behind each one adds
    its own suffix. The routes come from internal/api/router.go, parsed, not
    restated: a hard-coded list would keep passing after somebody moved a
    route or reordered the Anthropic exact route behind the OpenAI catch-all.

    Prevents: an SDK call that reaches a 404 instead of the model.
    """
    paths = _sdk_base_paths(sdk_client_ast)

    # The suffix table is only valid while each LangChain class still gets the
    # base path this gate believes it gets. Prove that from the SDK AST first.
    get_llm = _function(sdk_client_ast, "get_llm")
    anthropic_if = _anthropic_branch(get_llm)
    # Only the taken branch, never the whole If: ast.walk() on an If node also
    # walks its else body, so the two branches would be read as one and the
    # binding check would prove nothing.
    assert _base_url_attribute(_as_block(anthropic_if.body)) == "allm_path", (
        "ChatAnthropic no longer gets allm_path. The /v1/messages suffix "
        "below would be appended to the wrong base path."
    )
    assert _base_url_attribute(_as_block(anthropic_if.orelse)) == "llm_path", (
        "ChatOpenAI no longer gets llm_path."
    )
    assert _base_url_attribute(_function(sdk_client_ast, "get_embeddings")) == (
        "llm_path"
    ), "OpenAIEmbeddings no longer gets llm_path."

    # Endpoints that the SDK source itself builds, for example
    # self.image_generation_url = f"{self.base_url}{self.llm_path}/images/...".
    wanted: Dict[str, str] = {}
    for node in ast.walk(sdk_client_ast):
        if not isinstance(node, ast.JoinedStr):
            continue
        names, tail = _joined_str_parts(node)
        if not names or names[-1] not in paths or not tail:
            continue
        wanted[paths[names[-1]] + tail] = "SDK source f-string"

    # The scan above is the part that can degrade without a sound: an SDK that
    # builds its URL differently gives an empty result, and an empty result
    # makes the membership test below true for nothing. Demand at least one.
    assert wanted, (
        "no endpoint was read out of the SDK source f-strings. The client "
        "builds its LLM URLs some other way now."
    )

    for attribute, suffixes in LIBRARY_SUFFIXES.items():
        for suffix in suffixes:
            wanted[paths[attribute] + suffix] = "LangChain suffix on " + attribute

    missing = sorted(
        "{path} ({origin})".format(path=path, origin=origin)
        for path, origin in wanted.items()
        if not _router_serves(mounted_routes, path)
    )
    assert not missing, (
        "the SDK calls these paths, but {router!s} mounts none of them:\n"
        "  {missing}\nmounted: {mounted}".format(
            router=ROUTER_GO,
            missing="\n  ".join(missing),
            mounted=sorted(mounted_routes),
        )
    )


# ---------------------------------------------------------------------------
# 5. The reader logic itself
# ---------------------------------------------------------------------------


def _type_match_branch(function: ast.FunctionDef) -> ast.If:
    """Find the "if detail.get('type') == BUDGET_ERROR_TYPE:" statement."""
    for node in ast.walk(function):
        if not isinstance(node, ast.If):
            continue
        test = node.test
        if not isinstance(test, ast.Compare):
            continue
        left = test.left
        if not isinstance(left, ast.Call):
            continue
        if not isinstance(left.func, ast.Attribute) or left.func.attr != "get":
            continue
        if not left.args or not isinstance(left.args[0], ast.Constant):
            continue
        if left.args[0].value != "type":
            continue
        return node
    pytest.fail(
        "budget_exceeded_from() no longer tests <body>.get('type') in a plain "
        "comparison. Read the new logic and rewrite this gate; do not delete "
        "it.",
        pytrace=False,
    )
    raise AssertionError  # unreachable


def test_reader_matches_on_type_alone_and_reads_the_scope_from_code(
    sdk_exceptions: Dict[str, object], sdk_exceptions_ast: ast.Module
) -> None:
    """Pin the two rules the gateway body shape is built around.

    The structure is checked, and then the behaviour. Both are needed. The
    shipped defect had a correct structure: only the two values sat in the
    wrong fields. A structural check alone cannot see that, and a behavioural
    check alone cannot see the word "and" appearing in the test one day.
    """
    error_type = sdk_exceptions["BUDGET_ERROR_TYPE"]
    scopes = sdk_exceptions["BUDGET_SCOPES"]
    budget_exceeded_from = sdk_exceptions["budget_exceeded_from"]

    # --- structure -------------------------------------------------------
    branch = _type_match_branch(_function(sdk_exceptions_ast, "budget_exceeded_from"))
    test = branch.test
    assert isinstance(test, ast.Compare) and len(test.ops) == 1, (
        "the type test is now a chained comparison. It must stay one "
        "comparison on error.type alone."
    )
    assert isinstance(test.ops[0], ast.Eq), "the type test is no longer an equality."
    right = test.comparators[0]
    right_value = (
        right.value if isinstance(right, ast.Constant)
        else "BUDGET_ERROR_TYPE" if isinstance(right, ast.Name) and right.id == "BUDGET_ERROR_TYPE"
        else None
    )
    assert right_value in (error_type, "BUDGET_ERROR_TYPE"), (
        "the type test compares against {right!r}, which is neither "
        "BUDGET_ERROR_TYPE nor its value.".format(right=ast.dump(right))
    )

    scope_arguments = []
    for node in ast.walk(_as_block(branch.body)):
        if not isinstance(node, ast.Call):
            continue
        if not isinstance(node.func, ast.Name) or node.func.id != "BudgetExceededError":
            continue
        candidates = list(node.args[1:]) + [
            keyword.value for keyword in node.keywords if keyword.arg == "scope"
        ]
        scope_arguments.extend(candidates)
    assert len(scope_arguments) == 1, (
        "expected one BudgetExceededError scope argument in the type branch, "
        "found {count}.".format(count=len(scope_arguments))
    )
    scope_argument = scope_arguments[0]
    assert (
        isinstance(scope_argument, ast.Call)
        and isinstance(scope_argument.func, ast.Attribute)
        and scope_argument.func.attr == "get"
        and scope_argument.args
        and isinstance(scope_argument.args[0], ast.Constant)
        and scope_argument.args[0].value == "code"
    ), (
        "the scope no longer comes from error.code but from {source}. The "
        "gateway puts the scope in error.code.".format(
            source=ast.dump(scope_argument)
        )
    )

    # --- behaviour -------------------------------------------------------
    for shape_name, shape in sorted(BODY_SHAPES.items()):
        # error.type alone matches: a body with no code at all is still a
        # budget refusal.
        no_code = budget_exceeded_from(FakeUpstreamError(shape(error_type, None)))
        assert no_code is not None, (
            "a body with the right type and no code is no longer recognised "
            "({shape} shape). The type does not match alone any more.".format(
                shape=shape_name
            )
        )

        # error.code alone does NOT match. This is the shipped defect: the
        # gateway put the scope in error.type, and the SDK returned None.
        for scope in scopes:
            wrong = FakeUpstreamError(shape(scope, scope))
            assert error_type not in str(wrong), (
                "the fake exception text now carries the contract string, so "
                "the SDK text path could answer instead of the body path."
            )
            assert budget_exceeded_from(wrong) is None, (
                "the SDK now recognises a body whose type is {scope!r} "
                "({shape} shape). This gate was written because it does "
                "not.".format(scope=scope, shape=shape_name)
            )

        # The scope is read from error.code, for every declared scope.
        for scope in scopes:
            error = budget_exceeded_from(FakeUpstreamError(shape(error_type, scope)))
            assert error is not None
            assert error.scope == scope, (
                "error.code {scope!r} produced scope {got!r} ({shape} "
                "shape).".format(scope=scope, got=error.scope, shape=shape_name)
            )


def test_the_pin_names_the_content_of_every_sdk_file_this_gate_reads() -> None:
    """The pinned content list must cover exactly the files this gate reads.

    ISSUE #567. The pin identified the SDK by revision alone, so a dirty
    checkout minted a green pin. The content digests close that, and they close
    it only for the files they name. This test holds the two lists together:
    add a file to GATE_SDK_FILES and the pin must state its digest in the same
    change.

    It needs no SDK, so it runs even when the gates below skip. That matters:
    the digest comparison itself lives in the sdk_root fixture, where the tree
    is available, and a local developer without a checkout would otherwise see
    nothing of this gate at all.
    """
    pinned = set(_pinned_contents())
    read_here = {path.as_posix() for path in GATE_SDK_FILES}

    unpinned = sorted(read_here - pinned)
    assert not unpinned, (
        "this gate reads {unpinned} and {pin} does not state their content. "
        "The gate would then measure bytes the pin never recorded, which is "
        "the hole #567 closed for the other files.".format(
            unpinned=unpinned, pin=GATEWAY_SDK_PIN.name
        )
    )

    unread = sorted(pinned - read_here)
    assert not unread, (
        "{pin} states the content of {unread}, and this gate does not read "
        "them. A digest nobody checks is not a gate; either read the file or "
        "remove its entry.".format(pin=GATEWAY_SDK_PIN.name, unread=unread)
    )


# ── The gate's own floor ──────────────────────────────────────────────────────
#
# Tier 2 refuses a run that reports fewer than EXPECTED_ASSERTIONS results, and
# tier 3 does the same. Tier 1 had no such floor: renaming one test function to
# a name pytest does not collect produced "4 passed", exit 0, and the whole
# route-coverage half of this gate silently stopped being measured. A gate that
# reports success for the tests it no longer runs is the defect class this file
# exists to catch, so it checks itself the same way.
EXPECTED_TESTS = frozenset(
    {
        "test_sdk_error_type_equals_the_gateway_error_type",
        "test_gateway_codes_produce_exactly_the_sdk_scopes",
        "test_project_code_relies_on_the_sdk_default_scope",
        "test_sdk_llm_paths_are_mounted_by_the_gateway_router",
        "test_reader_matches_on_type_alone_and_reads_the_scope_from_code",
        "test_the_pin_names_the_content_of_every_sdk_file_this_gate_reads",
    }
)


def test_every_gate_in_this_file_is_still_collected() -> None:
    """Fail when a test in this file is renamed, removed, or shadowed.

    This test does NOT need the SDK, so it runs even when the others skip.
    Reading the file with ast rather than the imported module is what lets it
    run at all when the SDK is absent: introspecting globals() would require
    importing a module whose fixtures cannot resolve.

    WHAT IT CANNOT SEE. A source-level check cannot tell whether pytest RAN a
    test. A skip marker, an xfail, `-k`, `--deselect` and a collection error
    all leave the `def` exactly where it is; a module-level
    `pytestmark = pytest.mark.skip` makes this very floor skip itself and the
    run still exits 0. Counting what pytest REPORTED is a different job, and
    scripts/contract/conftest.py does it under CI.
    """
    source = Path(__file__).read_text(encoding="utf-8")
    tree = ast.parse(source)
    # Count the definitions, do not collapse them into a set. Python binds the
    # LAST definition of a name, so a duplicate `def` makes every earlier body
    # dead code that pytest never runs — and a set counts that name once and
    # reports a pass for a gate whose body was replaced. That is this file's
    # own failure class, and it applied to this floor.
    defined = Counter(
        node.name
        for node in tree.body
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
        and node.name.startswith("test_")
    )
    shadowed = sorted(name for name, count in defined.items() if count > 1)
    assert not shadowed, (
        "these test names are defined more than once in this file: {0}. Python "
        "binds the LAST definition, so every earlier body is dead code that "
        "pytest never runs.".format(shadowed)
    )
    collected = set(defined)

    missing = sorted(EXPECTED_TESTS - collected)
    assert not missing, (
        "these gates are named in EXPECTED_TESTS but pytest cannot collect "
        "them: {missing}. A renamed or deleted test reports a pass for a "
        "comparison that never ran. Restore the test, or remove its name from "
        "EXPECTED_TESTS in the same commit and say why.".format(missing=missing)
    )

    # A new test is welcome, but it must be named here, so the floor keeps
    # matching what the file actually measures.
    unlisted = sorted(collected - EXPECTED_TESTS - {"test_every_gate_in_this_file_is_still_collected"})
    assert not unlisted, (
        "these tests exist but are not named in EXPECTED_TESTS: {unlisted}. "
        "Add them, so a later rename cannot remove them unnoticed.".format(
            unlisted=unlisted
        )
    )
