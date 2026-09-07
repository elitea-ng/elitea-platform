"""Sidecar settings, strict-parsed from the environment.

Strict-parsed means an unparsable value raises at startup rather than silently
falling back to a default — the same rule ``elitea_deepwiki.config`` states, and
for the same reason: the legacy plugin's ``int(os.environ.get(..., "3"))`` inside
a bare ``except`` turned a mistyped variable into an HTTP 500 at request time
instead of a boot failure.

Deliberately ABSENT, because ADR-0023 H4c stage I3 does not port them:

``INVENTORY_JOBS_ENABLED``
    v1 runs ingestion in this sidecar, under the Go host's invocation manager
    and slot accounting. There is no Kubernetes Job, no worker image and no
    namespace to create one in. A setting that could be set and would do
    nothing is worse than none.
``PLATFORM_API_URL`` / ``AI_RUN_PLATFORM_TOKEN``
    the legacy admin token. Every platform read this engine performs — the
    graph object, the source repository, the embeddings, the LLM — is
    authorised by the per-invocation credentials the facade puts in
    ``llm_settings``. There is no admin credential in this process.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field

ENV_PREFIX = "ELITEA_INVENTORY_"

#: The source toolkit types v1 can ingest from. An expanded source of any other
#: type is refused BY NAME rather than attempted: the engine builds the SDK
#: toolkit from `type` + `settings`, and a type whose settings shape it has
#: never been run against fails somewhere inside the SDK with a message about a
#: missing key, which reads as a platform bug rather than as "not supported
#: yet".
DEFAULT_SOURCE_TYPES = ("github", "ado_repos")


class ConfigError(RuntimeError):
    """Raised when the environment cannot be parsed."""


def _raw(name: str, default: str | None = None) -> str | None:
    value = os.environ.get(ENV_PREFIX + name)
    return default if value is None else value


def _seconds(name: str, default: float) -> float:
    raw = _raw(name)
    if raw is None or raw == "":
        return default
    try:
        value = float(raw)
    except ValueError as exc:
        raise ConfigError(
            f"{ENV_PREFIX}{name} must be a number of seconds, got {raw!r}"
        ) from exc
    if value < 0:
        raise ConfigError(f"{ENV_PREFIX}{name} must not be negative, got {raw!r}")
    return value


def _choice(name: str, default: str, allowed: tuple[str, ...]) -> str:
    raw = _raw(name)
    if raw is None or raw == "":
        return default
    value = raw.strip()
    if value not in allowed:
        raise ConfigError(
            f"{ENV_PREFIX}{name} must be one of {list(allowed)}, got {raw!r}"
        )
    return value


def _csv(name: str, default: tuple[str, ...]) -> tuple[str, ...]:
    raw = _raw(name)
    if raw is None or raw.strip() == "":
        return default
    values = tuple(item.strip().lower() for item in raw.split(",") if item.strip())
    if not values:
        raise ConfigError(
            f"{ENV_PREFIX}{name} was set to {raw!r}, which names no source type. "
            f"Leave it unset for the default ({', '.join(default)})."
        )
    return values


@dataclass(frozen=True)
class Settings:
    """Everything the engine sidecar reads from the environment."""

    #: Ephemeral scratch: graph caches, checkpoints and the source status file
    #: between an ingestion and its upload. Nothing durable lives here — the
    #: graph's home is the platform artifact bucket, and this pod's copy is a
    #: cache that a restart may lose.
    scratch_path: str = "/var/scratch/inventory"

    #: The Unix socket this sidecar listens on for the Go host in the same pod.
    engine_socket: str = "/run/inventory/engine.sock"

    #: Which tool runner to serve. ``unavailable`` refuses every tool;
    #: ``legacy`` dispatches into the copied engine and needs the ``engine``
    #: extra. The default is the refusing one, so an image built without the
    #: closure cannot look like it has an engine.
    runner: str = "unavailable"

    #: How long the ``fixture`` runner pauses between progress steps.
    fixture_step_seconds: float = 0.0

    #: Where the ``fixture`` runner reads its canned graph from — a directory
    #: shaped like ``conformance/provider/fixtures/inventory`` (i.e. it must
    #: contain ``spi/graph.json``). Unset (the default) means the package's
    #: own bundled copy, ``elitea_inventory/fixtures/inventory`` — a wheel
    #: cannot read outside itself, so that copy is what a built image serves
    #: with no other configuration. Setting this lets an operator point the
    #: standalone-full compose overlay at the repository's own fixture
    #: directory instead — see ``fixture_graph.FixtureGraph.load`` and
    #: ``deploy/docker-compose.standalone-full.yml``'s ``INVENTORY_FIXTURES``.
    fixtures_path: str | None = None

    #: Source toolkit types this deployment may ingest from.
    source_types: tuple[str, ...] = field(default=DEFAULT_SOURCE_TYPES)

    #: The CA to trust when reading a graph back from the platform's artifact
    #: routes. The same variable the Go host in front of this reads for its
    #: UPLOAD side, so both halves of one hop trust the same authority.
    #:
    #: Unset means the system trust store — never "verify nothing". The legacy
    #: plugin passed `verify=False` on every platform call, which is how a
    #: bearer token is handed to whatever answers the address.
    tls_ca_file: str | None = None

    @classmethod
    def from_env(cls) -> "Settings":
        return cls(
            scratch_path=_raw("SCRATCH_PATH", "/var/scratch/inventory"),
            engine_socket=_raw("ENGINE_SOCKET", "/run/inventory/engine.sock"),
            runner=_choice("RUNNER", "unavailable", ("unavailable", "legacy", "fixture")),
            fixture_step_seconds=_seconds("FIXTURE_STEP_SECONDS", 0.0),
            fixtures_path=_raw("FIXTURES") or None,
            source_types=_csv("SOURCE_TYPES", DEFAULT_SOURCE_TYPES),
            tls_ca_file=_raw("TLS_CA_FILE") or None,
        )
