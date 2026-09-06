"""A wiki source that is an ARTIFACT FOLDER, not a git repository.

WHAT THIS IS. The engine indexes a DIRECTORY. Until now the only way to get
one was a clone: ``LocalRepositoryManager.get_repository_local_path`` builds a
``GitCloneConfig``, asks the remote for its HEAD, and runs ``git clone``. This
module produces the same directory from a folder in the invoking project's
artifact store, so everything after that point — file discovery, the graph
build, the vector store, the manifest, the chat pins — is unchanged.

THE SOURCE IS NAMED IN THE REPOSITORY STRING. A source is
``artifact://{bucket}`` or ``artifact://{bucket}/{prefix}``. That spelling is
deliberate: the repository string is the ONE value that already travels
end to end — browser, facade, host ``repo_config``, worker payload, indexer,
cache key — so a scheme in it needs no new field in any of those layers, and
no layer can mistake it for ``owner/repo``.

THE IDENTITY IS THE LISTING. Git gives a clone two things this does not: the
remote HEAD sha, which becomes the ``commit8`` segment of the cache identity,
and ``git branch --show-current``. Per-file history is never used — the
indexer walks the tree once. So a commit-SHAPED identity is enough, and the
one used here is ``sha256`` over the sorted listing: one line per object,
``key``, ``size`` and ``modified``. It has the property that matters — it
changes exactly when the folder changes — so a changed folder gets a new cache
path, a new ``repo_identifier`` and therefore a new generation, while an
unchanged folder reuses the last one.

THE CREDENTIAL IS THE CALLBACK BEARER THE INVOCATION ALREADY CARRIES. Reads go
through the platform's object API with the same actor identity the facade
mints for artifact WRITES (``llm_settings.api_base`` / ``api_key`` /
``organization``, read by ``engine.artifacts_platform_client``). That is also
the whole authorization story: the project id comes from the minted grant, not
from the request, so ``artifact://{bucket}`` can only ever name a bucket of
the caller's own project. There is no host to check against the git egress
allowlist because there is no external host.

WHY A CHILD ENVIRONMENT AND NOT ``os.environ``. A generation runs in its own
worker process (``run_in_subprocess`` defaults to true, and the frozen
artifact set exists only on that path). One process serves several
invocations, so writing the bearer into the SERVER's environment would let one
invocation materialise a folder with another invocation's token. The
credentials are placed in the CHILD's environment instead — see
``child_environment`` — which is per invocation by construction.

NOT WIRED FOR THE K8s JOBS RUNNER, and that is stated rather than discovered.
``jobs.PortedJobManager._job_env`` drops every forwarded name ending in
``_KEY``, so ``DEEPWIKI_ARTIFACT_API_KEY`` cannot reach a Job pod: ADR-0022
requires a projected file there, and nothing reads
``ELITEA_DEEPWIKI_CREDENTIALS_DIR`` yet. That gap is older than this module —
a Job cannot UPLOAD a wiki either — and closing it is the same piece of work
for both. An artifact source in jobs mode refuses with the message
``artifact_client`` raises, which names what is missing.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import re
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional

logger = logging.getLogger(__name__)

#: The scheme that makes a repository string an artifact folder.
ARTIFACT_SCHEME = "artifact://"

#: The provider type the two ``repo_config`` extractors emit for one.
ARTIFACT_PROVIDER_TYPE = "artifact"

#: elitea-main's bucket rule, reproduced so a bad bucket is refused before a
#: request is made rather than as an opaque 404
#: (``internal/api/v2/artifacts/handler.go``).
BUCKET_PATTERN = re.compile(r"^[a-z][a-z0-9-]{1,62}$")

#: elitea-main's object-key rules (``internal/infra/storage/ref.go``): no
#: leading or trailing slash, no ``.`` or ``..`` segment, at most 1024 bytes.
MAX_KEY_BYTES = 1024

#: Caps. They are not a policy about how big a wiki may be — they are the
#: reason a mistyped bucket cannot fill a worker's scratch disk. Both are
#: deliberately generous and both are overridable.
MAX_FILES_ENV = "ELITEA_DEEPWIKI_ARTIFACT_MAX_FILES"
MAX_BYTES_ENV = "ELITEA_DEEPWIKI_ARTIFACT_MAX_BYTES"
DEFAULT_MAX_FILES = 5000
DEFAULT_MAX_BYTES = 512 * 1024 * 1024

#: The marker written BESIDE a materialised directory, never inside it: a file
#: inside would be discovered and indexed as repository content.
MARKER_SUFFIX = ".source.json"


class ArtifactSourceError(ValueError):
    """A source this service will not materialise. The caller can fix it."""


@dataclass(frozen=True)
class ArtifactSource:
    """One artifact folder: a bucket, and an optional folder prefix."""

    bucket: str
    #: Normalised WITHOUT a trailing slash. "" means the whole bucket.
    prefix: str = ""

    @property
    def url(self) -> str:
        return ARTIFACT_SCHEME + self.bucket + (f"/{self.prefix}" if self.prefix else "")

    @property
    def list_prefix(self) -> str:
        """The prefix sent to the listing route.

        The trailing slash is what makes it a FOLDER. Listing on the bare
        ``docs`` would also return ``docs-archive/…``, which is a different
        folder that happens to share a spelling.
        """
        return f"{self.prefix}/" if self.prefix else ""

    @property
    def slug(self) -> str:
        """A filesystem-safe name for the cache directory."""
        raw = self.bucket + ("_" + self.prefix if self.prefix else "")
        return re.sub(r"[^A-Za-z0-9._-]+", "_", raw)


def is_artifact_source(repository: Any) -> bool:
    """Report whether a repository string names an artifact folder."""
    return isinstance(repository, str) and repository.strip().lower().startswith(
        ARTIFACT_SCHEME
    )


def parse_artifact_source(repository: str) -> ArtifactSource:
    """Parse ``artifact://bucket[/prefix]``, refusing anything else.

    The bucket and the prefix are validated HERE, against elitea-main's own
    rules, so a malformed source is refused before a credential is read.
    """
    if not is_artifact_source(repository):
        raise ArtifactSourceError(
            f"{repository!r} is not an artifact source; expected "
            f"{ARTIFACT_SCHEME}bucket[/prefix]"
        )
    remainder = repository.strip()[len(ARTIFACT_SCHEME):]
    bucket, _, prefix = remainder.partition("/")
    bucket = bucket.strip().lower()
    if not BUCKET_PATTERN.match(bucket):
        raise ArtifactSourceError(
            f"{bucket!r} is not a usable bucket name: a bucket starts with a "
            "letter and holds 2 to 63 lowercase letters, digits or hyphens"
        )
    return ArtifactSource(bucket=bucket, prefix=_normalise_prefix(prefix))


def _normalise_prefix(prefix: str) -> str:
    prefix = (prefix or "").strip().strip("/")
    if not prefix:
        return ""
    _refuse_unsafe_key(prefix, "folder prefix")
    return prefix


def _refuse_unsafe_key(key: str, what: str) -> None:
    if len(key.encode("utf-8")) > MAX_KEY_BYTES:
        raise ArtifactSourceError(f"{what} is longer than {MAX_KEY_BYTES} bytes")
    if "\x00" in key or "\\" in key:
        raise ArtifactSourceError(f"{what} {key!r} holds a character a key may not hold")
    for segment in key.split("/"):
        if segment in ("", ".", ".."):
            raise ArtifactSourceError(
                f"{what} {key!r} holds an empty or relative segment"
            )


# ---------------------------------------------------------------------------
# The listing, and the identity derived from it
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class ArtifactObject:
    key: str
    size: int
    modified: str
    #: The path the object takes inside the materialised directory.
    relative_path: str


def listing_digest(objects: Iterable[ArtifactObject]) -> str:
    """The sha256 that stands in for a commit sha.

    Sorted, so the listing's own order — a server's paging detail — cannot
    change the identity of an unchanged folder. ``modified`` is included
    because an object rewritten with the same length is a changed folder, and
    the listing route reports no content hash to tell the two apart.
    """
    digest = hashlib.sha256()
    for entry in sorted(objects, key=lambda item: item.key):
        digest.update(f"{entry.key}\x00{entry.size}\x00{entry.modified}\n".encode("utf-8"))
    return digest.hexdigest()


def collect_objects(
    source: ArtifactSource, listing: Iterable[Dict[str, Any]]
) -> List[ArtifactObject]:
    """Turn a raw listing into the objects to download, refusing the unsafe.

    Every key is checked against the same rules elitea-main enforces on the
    way IN, and against the prefix it was listed under. A key that escapes
    either is refused rather than skipped: a source that produced one is not a
    source whose remainder can be trusted.
    """
    prefix = source.list_prefix
    objects: List[ArtifactObject] = []
    for item in listing:
        key = str(item.get("name") or "").strip()
        if not key or key.endswith("/"):
            # A folder placeholder holds no bytes to index.
            continue
        _refuse_unsafe_key(key, "object key")
        if prefix and not key.startswith(prefix):
            raise ArtifactSourceError(
                f"object key {key!r} is not inside the folder {prefix!r}"
            )
        relative = key[len(prefix):]
        if not relative:
            continue
        objects.append(
            ArtifactObject(
                key=key,
                size=_as_int(item.get("size")),
                modified=str(item.get("modified") or ""),
                relative_path=relative,
            )
        )
    return objects


def _as_int(value: Any) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return 0


def check_caps(objects: List[ArtifactObject], source: ArtifactSource) -> None:
    """Refuse a folder that is too large to materialise, saying which cap."""
    max_files = _int_from_env(MAX_FILES_ENV, DEFAULT_MAX_FILES)
    max_bytes = _int_from_env(MAX_BYTES_ENV, DEFAULT_MAX_BYTES)
    if not objects:
        raise ArtifactSourceError(
            f"the artifact folder {source.url} holds no objects to index"
        )
    if len(objects) > max_files:
        raise ArtifactSourceError(
            f"the artifact folder {source.url} holds {len(objects)} objects, "
            f"over the limit of {max_files} ({MAX_FILES_ENV})"
        )
    total = sum(entry.size for entry in objects)
    if total > max_bytes:
        raise ArtifactSourceError(
            f"the artifact folder {source.url} holds {total} bytes, over the "
            f"limit of {max_bytes} ({MAX_BYTES_ENV})"
        )


def _int_from_env(name: str, fallback: int) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return fallback
    try:
        value = int(raw)
    except ValueError:
        logger.warning("%s=%r is not a number; using %d", name, raw, fallback)
        return fallback
    return value if value > 0 else fallback


# ---------------------------------------------------------------------------
# Materialising
# ---------------------------------------------------------------------------


def _destination_for(cache_dir: str, source: ArtifactSource, branch: str, digest: str) -> str:
    """The clone-shaped path: name, branch and the first 8 of the identity.

    The branch is a caller's string and it lands in a filesystem path, so it
    is folded to safe characters first. A version label holding ``..`` would
    otherwise choose the directory that gets written and later removed.
    """
    label = re.sub(r"[^A-Za-z0-9._-]+", "_", branch) or "main"
    return os.path.join(cache_dir, f"{source.slug}_{label}_{digest[:8]}")


def _write_file(root: Path, relative: str, data: bytes) -> None:
    """Write one object, refusing any path that leaves the destination.

    The key rules already refuse ``..``; this is the second check, made
    against the RESOLVED path, because the first one is a rule about text and
    this one is a fact about the filesystem.
    """
    target = (root / relative).resolve()
    if root.resolve() not in target.parents and target != root.resolve():
        raise ArtifactSourceError(f"object {relative!r} would be written outside the source")
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(data)


def materialise_artifact_source(
    repository: str,
    branch: str = "main",
    cache_dir: Optional[str] = None,
    *,
    force: bool = False,
    client: Any = None,
) -> str:
    """Download an artifact folder into a local directory and return its path.

    This is the whole substitution the engine needs: it returns the same thing
    ``get_repository_local_path`` returns for a clone.
    """
    source = parse_artifact_source(repository)
    branch = (branch or "main").strip() or "main"
    cache_dir = cache_dir or os.path.join("/tmp", "wiki_builder_repos")
    client = client if client is not None else artifact_client()

    listing = client.list_artifacts(source.bucket, source.list_prefix)
    objects = collect_objects(source, listing)
    check_caps(objects, source)
    digest = listing_digest(objects)

    destination = _destination_for(cache_dir, source, branch, digest)
    marker = Path(destination + MARKER_SUFFIX)
    if not force and marker.is_file() and Path(destination).is_dir():
        logger.info("Using materialised artifact source: %s", destination)
        return destination

    # The marker is written LAST, so an interrupted download leaves a
    # directory the next run refuses to reuse rather than one it trusts.
    if marker.exists():
        marker.unlink()
    root = Path(destination)
    if root.exists():
        shutil.rmtree(root)
    root.mkdir(parents=True, exist_ok=True)

    total = 0
    for entry in objects:
        data = client.download_artifact(source.bucket, entry.key)
        _write_file(root, entry.relative_path, data)
        total += len(data)
    logger.info(
        "Materialised %s: %d objects, %d bytes into %s",
        source.url, len(objects), total, destination,
    )

    marker.write_text(
        json.dumps(
            {
                "local_path": destination,
                # The name the engine reads a commit sha out of. It is a
                # sha256 of the listing, not a sha1 of a tree, and it is here
                # under this name because that is the field every cache key
                # downstream is built from.
                "commit_hash": digest,
                "branch": branch,
                "remote_url": source.url,
                "bucket": source.bucket,
                "prefix": source.prefix,
                "object_count": len(objects),
                "total_bytes": total,
                "exists": True,
            },
            indent=2,
        ),
        encoding="utf-8",
    )
    return destination


def artifact_repository_info(local_path: str) -> Optional[Dict[str, Any]]:
    """The repository info for a materialised folder, or None for a clone.

    ``LocalRepositoryManager.get_repository_info`` asks git three questions,
    and a materialised folder is not a git tree: without this the answers are
    an error dict with no ``commit_hash``, every generation of that folder
    keys its cache on the literal string ``unknown``, and a changed folder
    silently reuses the previous index.
    """
    marker = Path(str(local_path) + MARKER_SUFFIX)
    if not marker.is_file():
        return None
    try:
        info = json.loads(marker.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        logger.warning("Unreadable artifact source marker at %s", marker)
        return None
    if not isinstance(info, dict):
        return None
    info["exists"] = os.path.exists(local_path)
    return info


# ---------------------------------------------------------------------------
# Credentials
# ---------------------------------------------------------------------------


def artifact_client() -> Any:
    """The platform artifact client this worker reads its source through.

    The same client the wiki's own artifacts are written with, built from the
    same environment the K8s Job path already injects.
    """
    from .engine.artifacts_platform_client import create_platform_client_from_env

    client = create_platform_client_from_env()
    if client is None:
        raise ArtifactSourceError(
            "an artifact source needs the platform artifact credentials, and "
            "this worker has none. They are placed in the worker's own "
            "environment by the invocation; an in-process run does not carry "
            "them, so generate with run_in_subprocess enabled."
        )
    return client


def child_environment(payload: Dict[str, Any]) -> Dict[str, str]:
    """The artifact credentials one worker child needs, or nothing.

    Returns an empty mapping unless the payload's own source is an artifact
    folder: a git generation must not carry a platform bearer it has no use
    for.
    """
    if not isinstance(payload, dict):
        return {}
    repo_config = payload.get("repo_config")
    repository = ""
    if isinstance(repo_config, dict):
        repository = str(repo_config.get("repository") or "")
    if not is_artifact_source(repository):
        return {}
    llm_settings = payload.get("llm_settings")
    if not isinstance(llm_settings, dict):
        return {}

    from .engine.artifacts_platform_client import (
        extract_artifact_settings,
        inject_artifact_env_vars,
    )

    settings = extract_artifact_settings(llm_settings)
    if not settings.get("base_url") or not settings.get("api_key"):
        return {}
    source = parse_artifact_source(repository)
    environment = inject_artifact_env_vars(settings, source.bucket)
    # The wiki's OWN artifacts keep their own bucket: this variable is read by
    # the worker's upload path as well, and pointing it at the source folder
    # would write the generated wiki back into the material it was made from.
    from .engine.artifacts_platform_client import ARTIFACT_BUCKET_ENV, get_artifact_bucket

    environment[ARTIFACT_BUCKET_ENV] = get_artifact_bucket()
    return {name: str(value) for name, value in environment.items() if value}


# ---------------------------------------------------------------------------
# The repo_config shape
# ---------------------------------------------------------------------------


def repo_config_for(source: ArtifactSource, branch: str = "main") -> Dict[str, Any]:
    """The normalised ``repo_config`` an artifact source produces."""
    return {
        "provider_type": ARTIFACT_PROVIDER_TYPE,
        "provider_config": {"bucket": source.bucket, "prefix": source.prefix},
        "repository": source.url,
        "branch": (branch or "main").strip() or "main",
        "project": None,
        "is_cloud": None,
    }


def source_from_configuration(configuration: Dict[str, Any]) -> Optional[ArtifactSource]:
    """Read a source out of an ``artifact_configuration`` block, or None."""
    if not isinstance(configuration, dict):
        return None
    bucket = str(configuration.get("bucket") or "").strip()
    if not bucket:
        return None
    prefix = str(configuration.get("prefix") or configuration.get("folder") or "")
    return parse_artifact_source(
        ARTIFACT_SCHEME + bucket + (f"/{prefix.strip('/')}" if prefix.strip("/") else "")
    )


__all__ = [
    "ARTIFACT_PROVIDER_TYPE",
    "ARTIFACT_SCHEME",
    "ArtifactObject",
    "ArtifactSource",
    "ArtifactSourceError",
    "artifact_client",
    "artifact_repository_info",
    "check_caps",
    "child_environment",
    "collect_objects",
    "is_artifact_source",
    "listing_digest",
    "materialise_artifact_source",
    "parse_artifact_source",
    "repo_config_for",
    "source_from_configuration",
]
