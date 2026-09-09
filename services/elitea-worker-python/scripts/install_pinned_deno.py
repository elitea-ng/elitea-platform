#!/usr/bin/env python3
"""Install a pinned, checksum-verified `deno` binary into the worker image.

The Python worker's sandbox internal tool (`pyodide`) is the SDK's
`PyodideSandboxTool`, which refuses to construct unless it finds either
`SANDBOX_SERVICE_URL` or a `deno` executable on `PATH`
(`elitea_worker.agents.internal_tools._sandbox_backend_available`). This
script gives the image the second of those two.

Deno publishes no PyPI/apt package this image could pin the ordinary way, so
this downloads the official GitHub release archive directly. That is an
unpinned download unless two things hold, and this script enforces both:

  * the URL names an EXACT version, never `latest`;
  * the downloaded bytes are hashed and compared against a digest recorded
    here as a constant, before anything is extracted from the archive. A
    digest mismatch is fatal — nothing partial is written to `--dest`.

No system package manager, `curl`, or `unzip` is added to the image for this:
the interpreter that already exists in the base image downloads (via
`urllib.request`) and extracts (via `zipfile`, stdlib) the one file this
worker needs.
"""

from __future__ import annotations

import argparse
import hashlib
import io
import os
import stat
import sys
import urllib.request
import zipfile

# Deno is MIT licensed. Raised from whatever the image previously carried (or,
# on a fresh image, this is the initial pin) for the CVEs fixed by 2.9.6 and
# below — see https://github.com/denoland/deno/security/advisories.
DENO_VERSION = "2.9.6"

# One release, two architectures: docker-bake.hcl builds this image for
# linux/amd64 and linux/arm64 only (see the `platforms` list next to the
# `elitea-worker-python` target), so those are the only two this script knows.
# MEASURED: downloaded from the v2.9.6 GitHub release and hashed locally
# against the release's own `*.zip.sha256sum` file before this was written.
_ARCHIVE_SHA256 = {
    "amd64": (
        "x86_64-unknown-linux-gnu",
        "394f07f4da2bebe6ce6f1e7ce0fa16429b29b08c35e3fac3fe25972676dff4b2",
    ),
    "arm64": (
        "aarch64-unknown-linux-gnu",
        "9a46afc6c392c7cd2ff71a31558935545b46408d0e87f7a86908c712721c046e",
    ),
}

_MEMBER_NAME = "deno"


def _download_verified(target_arch: str) -> bytes:
    try:
        deno_arch, expected_sha256 = _ARCHIVE_SHA256[target_arch]
    except KeyError:
        raise SystemExit(
            f"no pinned deno archive digest for TARGETARCH={target_arch!r}; "
            "add one to _ARCHIVE_SHA256 rather than falling back to an "
            "unverified download"
        ) from None

    url = (
        "https://github.com/denoland/deno/releases/download/"
        f"v{DENO_VERSION}/deno-{deno_arch}.zip"
    )
    with urllib.request.urlopen(url, timeout=120) as response:  # noqa: S310
        archive = response.read()

    actual_sha256 = hashlib.sha256(archive).hexdigest()
    if actual_sha256 != expected_sha256:
        raise SystemExit(
            f"deno archive digest mismatch for {target_arch}: "
            f"got {actual_sha256}, want {expected_sha256}. Refusing to "
            "extract an unverified archive."
        )
    return archive


def _extract_binary(archive: bytes, dest: str) -> None:
    with zipfile.ZipFile(io.BytesIO(archive)) as bundle:
        names = bundle.namelist()
        if names != [_MEMBER_NAME]:
            raise SystemExit(
                f"deno archive holds {names!r}, expected exactly "
                f"[{_MEMBER_NAME!r}]; refusing to extract an unexpected shape"
            )
        with bundle.open(_MEMBER_NAME) as source, open(dest, "wb") as target:
            target.write(source.read())


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--target-arch",
        required=True,
        choices=sorted(_ARCHIVE_SHA256),
        help="the image's TARGETARCH (amd64 or arm64)",
    )
    parser.add_argument(
        "--dest",
        required=True,
        help="path to write the verified deno binary to",
    )
    args = parser.parse_args(argv)

    archive = _download_verified(args.target_arch)
    _extract_binary(archive, args.dest)

    mode = stat.S_IRUSR | stat.S_IXUSR | stat.S_IRGRP | stat.S_IXGRP | stat.S_IROTH | stat.S_IXOTH
    os.chmod(args.dest, mode)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
