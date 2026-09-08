# elitea-inventory — the Inventory engine, as a sidecar

The knowledge-graph engine of the legacy `inventory_plugin`, ported to run
behind the Go sub-application host (ADR-0023 stage H4c, I3).

```
facade ──mTLS──▶ elitea-subapp-host (Go)  ──unix socket──▶  this (Python)
                 SPI, admission, merge,        NDJSON        the engine:
                 deferred refusals,                          ingest, retrieve,
                 composition, artifact upload                investigate
```

The host owns everything generic: the provider SPI, the invocation registry and
slots, the error contract, the parameter merge, result composition, and putting
artifacts into the platform bucket. This package owns the engine — and only the
engine.

## Layout

| path | what it is |
|---|---|
| `src/elitea_inventory/engine/` | the legacy analysis layer, **copied byte for byte** (`COPY_MANIFEST.json` pins every digest) |
| `src/elitea_inventory/tool_operations.py` | `methods/invoke.py`, transformed by declared substitutions |
| `src/elitea_inventory/chat_operations.py` | `methods/inventory_chat.py`, likewise — the agent behind `investigate` |
| `src/elitea_inventory/v1_overrides.py` | the six methods v1 **replaces** rather than transforms |
| `src/elitea_inventory/tools_table.py` | the legacy routing table, as data |
| `src/elitea_inventory/sidecar.py` | `/engine/invoke`, `/engine/invocations/{id}/stop`, `/engine/health` |
| `tools/refresh_engine_copy.py` | performs the copy and verifies it (`--check`) |
| `tools/build_descriptor_v1.py` | derives descriptor revision `legacy-v1` from `legacy-v0` |

## Running it

```bash
uv venv --python 3.12 .venv
# `[engine]` too: tests/engine imports the copied engine. Since I8 that closure
# is 126 exact pins rather than elitea-sdk's blanket extras, so it no longer
# drags torch in and plain pip resolves it (25s, measured) — which is what made
# a CI job affordable. `[test]` alone runs only the sidecar and runner suites;
# three engine files and one unit file need langchain_core:
#   PYTHONPATH=src .venv/bin/python -m pytest tests --ignore tests/engine
uv pip install --python .venv/bin/python -e '.[engine,test]'
PYTHONPATH=src .venv/bin/python -m pytest

# the sidecar on a socket, and a health probe over it
PYTHONPATH=src .venv/bin/python e2e/health_over_socket.py
```

`.github/workflows/ci-inventory.yml` runs exactly that suite on every pull
request touching this package, with the same two extras. Before it existed
these 305 tests — including the digest check that makes "the engine is a
byte-for-byte copy" mean something — ran on developer machines only, which is
why two defects in `sources.py` and the engine's chunker block stayed latent
until stage I8 went looking. The job also refuses a vacuous run: it counts the
collected tests and fails below a floor, so a suite that stops running fails
loudly instead of passing silently.

```bash
# the shell image: 221 MB, builds in a couple of minutes
podman build -f services/elitea-inventory/Containerfile \
    -t localhost/elitea-inventory:local-shell .

# the sidecar answering on a real Unix socket, inside that image
podman run --rm -v "$PWD/services/elitea-inventory/e2e:/e2e:ro" \
    --entrypoint python localhost/elitea-inventory:local-shell \
    /e2e/health_over_socket.py

# the engine image — see the caveat below
podman build -f services/elitea-inventory/Containerfile \
    --build-arg 'EXTRAS=[engine]' \
    -t localhost/elitea-inventory:local-engine .
```

**Build status, stated exactly.** The SHELL image is built and verified: 221 MB,
and `health_over_socket.py` run inside it serves `/engine/health`, refuses a
foreign tool at the transport with 400, and refuses a served tool in band on the
stream. That exercises the whole Containerfile — the uv lock, the wheel build,
the `--no-deps` install, the socket directory's ownership, the non-root user and
the entrypoint.

The ENGINE image is built and verified too, since I8: **1.35 GB**, built in a
few minutes. Run inside it, `health_over_socket.py` with
`ELITEA_INVENTORY_RUNNER=legacy` — the real engine, not the fixture — serves
`/engine/health`, refuses a foreign tool with 400, and answers `get_stats` in
band on the stream. A probe run in the same image imports every SDK symbol this
package names, instantiates both source toolkits, and runs both chunkers;
`chromadb`, `torch`, `unstructured`, `transformers`, `sentence-transformers`,
`timm` and `cv2` all report absent.

Before I8 this image could not be built at all: the wheel step resolved,
downloaded and built the whole closure and then failed writing the layer — `no
space left on device`, with ~18 GB free, because a wheelhouse containing torch,
its NVIDIA CUDA wheels and opencv does not fit.

`ELITEA_INVENTORY_RUNNER` defaults to `unavailable`, which refuses every tool
with a readable reason. `legacy` selects the engine and needs the `engine`
extra; `fixture` answers canned results for a stack that proves the socket hop
without the closure.

### Fixture mode, and the graph it replays

`ELITEA_INVENTORY_RUNNER=fixture` used to answer a sentence and an empty
graph — enough to prove the socket hop, useless for looking at the Inventory
application. It now replays the same canned graph (six entities, two source
toolkits, five relations) the Go sub-application host's own fixture runner
(`services/elitea-subapp-host/internal/apps/inventory/run/fixture.go`) serves
on the E2E stack, which has no engine sidecar at all — the two-fixture-runner
setup the DeepWiki port also has, and the trap the survey that scoped this
work names: nothing forces the two to agree.

The graph itself lives once, checked in, at
`conformance/provider/fixtures/inventory/spi/graph.json`; the answers for a
curated set of representative tool calls (search, an entity, its neighbours,
impact analysis, cross-source relations, stats, ingestion, the sources list)
are pinned the same way under `conformance/provider/fixtures/inventory/
{ingestion,retrieval}/*.json`. `tests/unit/test_fixture_parity.py` loads every
one of those files and asserts `elitea_inventory.fixture_graph` answers the
same shape; `services/elitea-subapp-host/internal/apps/inventory/run/
fixture_parity_test.go` loads the identical files against the Go runner. A
change to the graph that only edits one language's copy fails exactly one of
those two tests.

`elitea_inventory.fixture_graph.FixtureGraph.load()` reads
`<fixtures_dir>/spi/graph.json`. `ELITEA_INVENTORY_FIXTURES` names
`<fixtures_dir>`; unset (the default) it is this package's own bundled copy,
`src/elitea_inventory/fixtures/inventory` — a wheel cannot read outside
itself, so that copy is what a built image serves with no other
configuration. `tests/unit/test_fixture_graph.py` asserts the packaged copy
and the conformance one are byte-identical.

`deploy/docker-compose.standalone-full.yml`'s `elitea-inventory-engine`
service bind-mounts `conformance/provider/fixtures/inventory` read-only at
`/fixtures/inventory` and leaves `ELITEA_INVENTORY_FIXTURES` empty by
default (so the packaged copy answers); set `INVENTORY_FIXTURES=/fixtures/
inventory` before `deploy/scripts/standalone-stack.sh up` to read the
repository's own copy instead — useful for iterating on the fixture data
without rebuilding the image. See `deploy/README.md`'s compose section.

## The v1 scope, and what it deliberately does not port

**Sources: `github` and `ado_repos` only.** The facade expands the source
toolkit and forwards `{toolkit_id, type, name, settings, branch?, whitelist?,
blacklist?}` per invoke. The legacy plugin instead took a bare `toolkit_id` and
fetched that toolkit's expanded **credentials** itself, with an admin platform
token, on the strength of an id the caller supplied that nothing checked the
caller could see. There is no admin credential in this process at all;
`ELITEA_INVENTORY_SOURCE_TYPES` widens the allowlist for a deployment that has
actually run the engine against another type.

**No Kubernetes ingestion Jobs.** Ingestion runs here, under the host's
invocation manager and slot accounting. `k8s_ingestion_job_manager.py`, the
worker image and `INVENTORY_JOBS_ENABLED` are not ported, and the setting is not
read in any spelling — a setting that can be set and does nothing is worse than
none.

**Embeddings through the platform gateway.** The legacy plugin embedded entities
with a local `all-MiniLM-L6-v2`, which needed runtime egress to a model host and
meant the one thing on this platform that embedded outside the gateway was
unmetered. (Removing it did not, on its own, make the image small — torch kept
arriving transitively until I8 took the SDK's blanket extras away. What it
changed is that this service no longer has a reason of its own to carry torch,
and nothing downloads a model at runtime.) v1 uses the toolkit's `embedding_model`
against `llm_settings.api_base`, and keeps the legacy code's *reasoning* about
drift by **recording** the model in the graph instead of pinning it in code: a
retrieval over a graph built in a different embedding space is refused by name,
which the legacy code could not do because it had nothing to compare.

**Chat is not ported** — the socket.io transport, `chat_history.json`, the
streaming callbacks. `investigate` **is**: it is one of the six
`inventory_search` tools, and it drives the same agent over `llm_settings`.

**Tools.** The `inventory` family declares 33 in revision `legacy-v1`, of which
28 are served; the five the legacy router never carried are refused by name at
the host. All 6 `inventory_search` tools are served. See
`conformance/provider/fixtures/inventory/descriptor/README.md`.

**Graph persistence.** `graph.json`, the per-source checkpoints and
`sources_status.json` are returned **inline** and uploaded by the host, through
the transport the facade handed over. Reading a graph back goes through
`elitea_inventory.artifacts`, authorised the same way — so a caller who cannot
see the bucket gets a 403 rather than someone else's graph. The local copy under
`ELITEA_INVENTORY_SCRATCH_PATH` is a cache; the bucket is the home.

## The copy, and why it is a copy

`engine/` is ~24k lines of graph algorithms, extractors, parsers and retrieval
with no platform coupling. Rewriting it would be a rewrite of the product, and a
rewrite is not checkable. A copy is — by digest:

```bash
python tools/refresh_engine_copy.py --check   # what CI runs; needs no legacy tree
python tools/refresh_engine_copy.py --source ../../legacy/plugins/inventory_plugin
```

The two **tool-layer** files cannot be verbatim: they import Pylon at module
scope, decorate every method with `@web.method()`, and read the invocation id
from a Pylon-injected module. The substitutions that remove those are declared
in the refresh tool and are the only thing applied; the manifest pins both the
result's digest and the digest of the legacy source it came from, so editing the
result by hand fails and the legacy source moving under us fails the next
re-copy.

Three copied *engine* files import Pylon's logger with no fallback. Rather than
grow the substitution list one entry per file — which is how a "verbatim" copy
quietly stops being one — `pylon_shim.py` registers a stub `pylon.core.tools`
providing exactly a stdlib logger and an inert decorator, and nothing else.

## Not yet done (stages I4–I7)

* **I4 — the facade.** `services/elitea-main` must expand the source toolkit and
  forward it per invoke. Until it does, every ingest call is refused by the host
  with "the facade does" — correctly, and unusably. Owned by another agent.
* **I5 — deploy wiring.** No Helm chart, no compose service, no `docker-bake.hcl`
  target and no `.github/image-scan-exempt.txt` entry. Nothing here needed one:
  the image builds from the Containerfile directly and the tests run without a
  stack.
* **I6 — the web UI.** The graph view, the sources view and the chat view all
  called the legacy plugin's own HTTP routes. Descriptor revision legacy-v1 is
  what makes their four tools reachable through the facade; nothing yet calls
  them.
* **I7 — a real-engine run.** Everything here is tested against injected tools
  or the copied engine's own unit tests. No ingestion has been run against a
  real repository through the socket, so the first one will find things — that
  is what DeepWiki's equivalent run found (a missing `git` binary, a
  root-owned socket directory, an empty manifest reported as success).
* **I8 — the engine image's weight. DONE.** See "The engine closure, after I8"
  below.

## The engine closure, after I8

The `engine` extra no longer names `elitea-sdk[runtime,tools]`. It names a bare
`elitea-sdk==0.9.40` plus an exact set, measured — the same shape
`services/elitea-worker-python`'s `indexing-current` uses when it
"intentionally replaces" the SDK's ranges. `pyproject.toml` carries the reason
for each member; this is the summary.

**The premise this section used to carry was wrong, and the correction is the
point.** It blamed the `tools` extra for torch and chromadb. Measured at the
gate's own target (Python 3.12, `x86_64-manylinux_2_28`):

| requirement | packages | carries |
|---|---|---|
| `elitea-sdk` (no extras) | 40 | nothing heavy |
| `elitea-sdk[runtime]` | 255 | chromadb, torch + its NVIDIA CUDA wheels, unstructured, timm, transformers, sentence-transformers, opencv-python |
| `elitea-sdk[runtime,tools]` + the old extra | 356 | all of the above |
| the extra as it stands now | **126** | none of the above |

`chromadb` is named by the SDK's **runtime** extra directly, and again by
`langchain-chroma`, which that extra also names. `torch` arrives under runtime
through `unstructured -> unstructured-inference/effdet/timm` and through
`sentence-transformers`. **Dropping only `tools` would have removed neither.**
230 distributions leave the resolution; none is new.

(The "~1,330-package closure" this file used to claim was a line count of an
annotated `uv pip compile` output — 1,333 lines, 356 packages.)

**What it closes.** Four open Dependabot alerts, all attributed to this
package's `pyproject.toml`, all `chromadb` 1.5.9, none with a patched version
published: `GHSA-36p7-vc44-83pf` and `GHSA-f4j7-r4q5-qw2c` (CRITICAL),
`GHSA-2wm9-hf6c-p5cr` and `GHSA-xph7-9rjv-w5fr` (high). Nothing here ever used
chromadb, and there was no version to move to, so removing it was the only
route open.

**How the set was found.** From a BARE `elitea-sdk`, one dependency at a time,
against a probe that does not stop at imports: it imports every SDK symbol this
package names, then INSTANTIATES both `DEFAULT_SOURCE_TYPES` toolkits through
`elitea_sdk.tools.instantiate_toolkit`, then RUNS both chunkers over a real
`.py` and a real `.md`. That distinction is load-bearing, because
`elitea_sdk.tools` imports every toolkit module and **swallows the failures** —
it logs `Failed imports: github, ado_repos, …` and carries on. A green import
proves nothing.

| added | what it unlocked |
|---|---|
| `langchain-core`, `langgraph` | `elitea_sdk.tools` imports at all |
| `PyGithub` | the `github` module the SDK's github toolkit imports |
| `langchain-text-splitters`, `tree-sitter` | nothing on their own |
| `tree-sitter-languages` — the OLD one; `tree-sitter-language-pack` does not satisfy it | `parse_code_files_for_db` RUNS: 2 chunks, `greet` and `main` |
| `setuptools` | `distutils`, which left the stdlib in 3.12 and the SDK still imports |
| `numpy` | `chunk_single_document` RUNS, on the `.py` and on the `.md` |
| `langchain-community`, then the loader chain: `python-dateutil`, `gensim`, `openpyxl`, `mammoth`, `markdownify`, `python-docx`, `pandas`, `xlrd`, `reportlab`, `svglib`, `pymupdf`, `python-pptx` | `document_loaders.constants` imports every loader at module scope, and both `elitea_sdk.tools.github` and `runtime.clients.client` import it |
| `psycopg[binary]` | `elitea_sdk.tools.github`, `langraph_agent`, `get_tools`. With bare `psycopg` the github toolkit is one of the SWALLOWED failures and the only visible symptom is `instantiate_toolkit` saying github "is not available"; the real message, inside `FAILED_IMPORTS`, is "no pq wrapper available" |
| `azure-devops` | `elitea_sdk.tools.ado.repos` |
| `langchain-openai`, `langchain-anthropic`, `langchain` | `EliteAClient` |

**The evidence, not the claim.** With the resolved 126-package set installed the
way the image installs it (`uv pip compile`, then `pip install --no-deps -r`):
all eight SDK imports resolve; `github` instantiates into 38 tools and an
`EliteAGitHubAPIWrapper`; `ado_repos` gets as far as `TF400813`, a credential
refusal **from Azure**, which means every import, model and HTTP client on its
path ran; both chunkers produce chunks. All 304 tests pass against it, and the
`-engine` image builds and serves `/engine/health`.

**What did NOT change.** Six transitive versions are pinned where the blanket
extra left them — `anthropic`, `azure-core`, `beautifulsoup4`,
`charset-normalizer`, `langgraph-checkpoint`, `lxml` — because removing 230
packages removed whatever held each of them down, and a re-resolution floated
them all upward at once. I8 changes what the image CARRIES; letting it also
change what the frozen engine RUNS AGAINST would put two changes behind one
measurement, and #677 is what that costs. `langsmith` moved 0.10.15 -> 0.11.2
inside its own documented `<0.12` bound and is left ranged.

**Two defects the probe found, both pre-existing. BOTH are now FIXED.**

* ~~`engine/inventory/ingestion.py:3060`~~ **fixed** — it does `from
  langchain.text_splitter import RecursiveCharacterTextSplitter`. That module
  does not exist in `langchain` 1.x, which is what this closure has resolved
  since before I8, so the whole `try:` block raised `ImportError`,
  `has_chunker` became `False`, and the streaming path chunked nothing: every
  document, at any size, reached the batch as one raw chunk while the run
  reported success. The chunkers themselves were fine — the third import beside
  them was not.

  The engine files are byte-frozen and this package's copy tool substitutes
  only the tool layer, so the import cannot be rewritten in place. Fixed the
  way the Pylon logger import was, with a shim
  (`elitea_inventory.langchain_shim`), with one difference: `pylon_shim`
  fabricates a module, this one ALIASES `langchain_text_splitters` — where
  LangChain moved the very code the old name exported — so the binding is a
  rename rather than a stub. It never displaces a real
  `langchain.text_splitter`, and when neither name is installed it binds
  nothing, leaving the engine's own `ImportError` fallback to be honest about
  it rather than fabricating a splitter that chunks nothing.

  Measured by `tests/unit/test_streaming_chunking.py`, which drives the
  streaming loop and asserts the CHUNKS handed to the batch processor, because
  nothing in `IngestionResult` distinguishes chunked from not: a
  15,028-character prose document arrived as 1 chunk before, and as many
  after.
* ~~`sources.build_toolkit`~~ **fixed** — it passed `{id, name, type,
  settings}` to `instantiate_toolkit`, but the SDK subscripts keys that dict
  did not carry. MEASURED against the pinned SDK, on the old shape:

  | source type | raised |
  |---|---|
  | `github` | `KeyError: 'active_branch'` |
  | `ado_repos` | `KeyError: 'toolkit_name'` |

  The SDK reads the name from `toolkit_name` (not `name`) and uses it as the
  pgvector collection name; `github`'s factory additionally requires
  `active_branch` and `base_branch` in `settings` with no defaults of its own,
  while `ado_repos` defaults both to `main`. `build_toolkit` now sends
  `toolkit_name`, sets `active_branch` from the requested branch — that is the
  branch the pipeline reads files from — and defaults `base_branch` only when
  the stored settings do not name one, since the repository's base branch is a
  different thing. Credentials are still never fabricated: an absent
  `<type>_configuration` is refused by name, where the SDK would have raised
  `KeyError` from inside the toolkit.

  After the fix, against the real SDK: `github` returns its API wrapper, and
  `ado_repos` reaches Azure DevOps and is refused there (`TF400813`) — a
  refusal from Azure is the whole dependency and credential path proved.

### A note on the dependency resolution

The engine image's builder stage resolves with **uv** and builds wheels with pip
(`--no-deps -r`). MEASURED over six image builds, BEFORE I8: pip could not
resolve this closure at all. The blanket extras brought 356 packages, and inside
that set chromadb, mcp, langsmith, `uvicorn[standard]`, unstructured-client and
their h11/httpcore/websockets constraints formed a search space pip explored one
metadata download at a time. Every build was killed while pip was still walking
backwards through some package's release history; pinning the offenders one at a
time removed one dimension per build and revealed the next.

At 126 packages pip CAN resolve it — `pip install -e '.[engine,test]'` completes
on a developer machine, which is how the 304-test suite was run against it. The
uv/pip split stays anyway: `--no-deps -r` means a missing wheel is an error in
the runtime stage rather than a silent fall back to a version nobody resolved.

The lock is generated inside the build rather than committed, so it cannot go
stale against `pyproject.toml`.

### The closure moves as a set, and Dependabot is silent on it

Same rule as `services/elitea-deepwiki`, same reason, and this package is where
it was proved twice over:

* **The pins move together, never one at a time.** #740 moved `fastapi` to
  0.141.1 in the same commit that moved `elitea-sdk` to 0.9.40, which pins
  `fastapi==0.115.9` exactly; #741 moved `mcp` to 2.1.1, and that SDK requires
  `mcp>=1.24,<2`. Either alone is a `ResolutionImpossible`. The `engine` extra
  did not resolve on `main` from 2026-09-02 until the gate below found it —
  nothing in CI resolved it, and no CI job builds this image.
* **The pins move together with the copy.** `pyproject.toml` carries a
  `# closure-stamp: COPY_MANIFEST.json sha256 …` line naming the engine copy
  the closure was resolved against; the gate refuses a tree where the copy
  moved and the stamp did not.
* **Dependabot is deliberately silent here.** `.github/dependabot.yml` gives
  this package its own pip entry with `ignore: "*"` and
  `open-pull-requests-limit: 0`. Alerts still appear in the Dependabot tab;
  the automatic one-pin-at-a-time pull request does not.

Validate — resolution only, no install and no image build:

```bash
bash scripts/ci/check-engine-closures.sh
```

It runs on every pull request and daily (the `engine-closures` job in
`.github/workflows/dependency-scanning.yml`), and it resolves this package with
**uv** at the same pinned version the `Containerfile` installs, for the reason
this section already gives.

Move the closure:

```bash
python3 scripts/ci/refresh-engine-closure.py services/elitea-inventory --check
```

For this package the tool **reports** the drift and does not rewrite. Its
`engine` extra deliberately mixes exact pins with ranges, and both kinds carry
paragraphs of measured reasoning beside them in `pyproject.toml` — `langsmith`'s
h11 bound, and the I8 ledger recording what each exact pin unlocked; a
mechanical rewrite would delete the reason along with the version number it
explains. So the edit here is a person's, made with the reported drift in hand,
and the acceptance gate is the `[engine]` image build.

The pins in `pyproject.toml` — exact `fastapi` and `uvicorn`, the measured I8
set, the six versions held where the blanket extra left them, and
`langsmith<0.12` — each record a real constraint with the measurement beside it,
and they are what a resolver of either kind lands on. The `langsmith` bound is
copied unchanged from
`services/elitea-worker-python`: it is one platform-wide fact (0.12 moves to
httpx2/httpcore2 and needs h11>=0.16, which cannot coexist with httpcore 1.0.7)
and two services disagreeing about it would be worse than either answer.
