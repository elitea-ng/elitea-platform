# elitea-doc-extractor

The document extraction sidecar of the Rust indexing worker (ADR-0030 I1).
It turns the PDF and Office documents an index run fetched into text, over
`elitea.extract.v1` on a Unix socket, in a process of its own.

## Why a separate process

Two reasons, and either alone would do.

1. **The worker's lock cannot hold the PDF backend.** `elitea-doc-extract`'s
   `documents` feature brings xberg 1.3.6, whose pure-Rust PDF backend
   (`xberg-native-pdf`) requires `smallvec >= 1.16`. The worker's
   `kube-client` 4.2.0 (its `sandbox-supervisor` feature) pins `serde-saphyr
   ^0.0.29`, which caps `smallvec < 1.16`. One `Cargo.lock` cannot satisfy
   both, and no kube release lifts the cap. This crate is its own Cargo
   workspace with its own lock; `ci-doc-extractor.yml` asserts the floor
   here and reports when the worker's lock ever moves.
2. **Untrusted bytes belong behind a process boundary.** A PDF a connector
   fetched is attacker-controlled input to a large parser. Here a panic, a
   runaway recursion or an OOM kill ends this process; the worker's client
   records the document `extract_failed`, reconnects, and goes on with the
   run.

## Contract

`libs/proto/elitea/extract/v1/extract.proto`. One client-streaming RPC,
`Extract`: a header (name, media type, declared length, per-document limits),
then the bytes in chunks; the answer is the text or a typed skip
(`TOO_LARGE`, `TOO_MANY_PAGES`, `ENCRYPTED`, `UNSUPPORTED`, `CORRUPT`,
`TIMEOUT`, `OUTPUT_TOO_LARGE`, `EMPTY`). A skip is a statement about the
document and travels as a response; a malformed stream, no capacity or an
internal failure is a gRPC status, which the worker records as
`extract_failed`. `Capabilities` lists the media types, the caps, the
capacity (`concurrency`, `slot_wait_millis`, `stream_idle_millis`) and the
present load (`busy`, `overruns`), and is the health probe: the binary's
`healthcheck` fails when `overruns` is every slot (see "Timeouts"). The
worker's client reads it once before its first document (again after any
failure) and shapes itself to it, below.

The worker side is `services/elitea-worker-rust/src/transport/extractor_client.rs`;
it is used by `src/indexing/pipeline.rs` for every media type
`elitea_doc_extract::is_document_type` names when `extractor_socket` is set
in the runtime document, and such documents stay `unsupported_type` when it
is not. The worker maps the skips onto its summary labels: `too_large`
(input or output), `too_many_pages`, `encrypted`, `unsupported_type`,
`unreadable` (corrupt), `extract_timeout`, `empty`; plus `extract_failed`
for the status case.

### Embedded pictures (ADR-0030 I4 part C)

A header may carry `images` (`ExtractImages`: a count, a per-picture size and
a total size; zero is the extractor's own cap). Only then, and only for a
PDF, DOCX or PPTX, the answer carries the document's embedded pictures beside
the text (`ExtractedText.images`): each with its bytes, the media type its
BYTES say (PNG, JPEG, GIF, WebP or BMP; anything else, SVG and masks
included, is counted in `images_skipped.unsupported`), its position in
document order, the page or slide, the package's file name where there is one
(`image1.png`) and `text_offset`, the byte offset in the text where its
description belongs. A header without `images` reads no picture, and the text
is the same either way (a test asserts it for all three formats).

The bytes are passed through: the extractor decodes and re-encodes nothing it
returns. DOCX and PPTX pictures are the package's own files; a PDF's come
from xberg's PDF backend, which hands them over as image files it wrote
(PNG), so the PDF parser's own decoding of its image streams is the one place
a picture is decoded here, inside the same process boundary as the rest of the
PDF parsing. xberg reads a picture into memory before the caps can drop it,
so the work is bounded BEFORE the parser runs: a DOCX or PPTX's media are
counted from the zip's central directory (nothing is decompressed) and a
package whose media outweigh four times the caps (in count or declared bytes)
gives its text and no picture (counted `too_many` / `too_large`). xberg's PDF
backend decodes the pictures of EVERY page in parallel, with no document
budget and no way to stop once enough are found, so a PDF is read in two
passes: the text alone first (which gives its page count), then, only for a
PDF of at most 8 pages, 16 MiB and 4 MiB of text, again with the pictures
(one a page kept, one more seen to show an overflow). A longer PDF gives its
text and no picture, counted as one `too_many` (how many it holds is not known
without decoding them); an image-only PDF over 8 pages therefore gives no
text and no picture, and is skipped `empty`. A document with no text but with
pictures (a scanned PDF of at most 8 pages, an image-only deck) is returned
with an empty text and its pictures: the worker indexes it from their
descriptions. With no picture to describe it is `empty` as before.

`text_offset` is recovered from what xberg's plain text gives, because that
text carries no marker for a picture: right after the picture's own
`[Image: ...]` line when the text has one (DOCX prints one per picture), else
the end of its page's or slide's text (the page is found in the whole by its
page text, in order), else the end of the text. See `doc-extract::anchors`.

Caps (a request is lowered to them; `Capabilities.image_limits` states them):

| Variable | Meaning | Default |
| --- | --- | --- |
| `ELITEA_EXTRACTOR_MAX_IMAGES` | pictures returned for one document | 24 |
| `ELITEA_EXTRACTOR_MAX_IMAGE_BYTES` | bytes of one returned picture | 5 MiB |
| `ELITEA_EXTRACTOR_MAX_IMAGE_TOTAL_BYTES` | bytes of all the pictures of one document | 12 MiB |

A returned picture is held about three times (the parser's, the response's,
the encoded message). A DOCX or PPTX's media may weigh four times the total
bound inside the parser; a PDF holds up to 8 pages x 2 pictures at the
per-picture bound (the parser has no byte bound of its own), its own bytes a
second time and its text three times. A slot costs up to 144 MiB more than its
document at the defaults (`Config::picture_memory`, times the real
concurrency), inside the quarter of the container limit a config test holds
it to; the test fails if a default grows past it.

## Transport and isolation

gRPC over a Unix domain socket in a directory the two containers share (a
Kubernetes `emptyDir`, a compose volume). No TCP port and no TLS: the
directory is the only way in, which is the engine sidecars' model
(`libs/rust/engine-sidecar`), and this is the **pod-local Unix-socket
sidecar exception** to the platform's gRPC-over-mTLS rule (AGENTS.md): gRPC
still, with the contract in `libs/proto`, but no TLS on a same-pod socket.
In its place the service checks the peer: every connection's credentials
(`SO_PEERCRED`) are read and only the uids in `ELITEA_EXTRACTOR_PEER_UIDS`
(the worker's; by default this process's own uid, which is the worker's in
every shipped deployment) are served `Extract` — any other peer gets
`PERMISSION_DENIED` before a byte of a document is read. `Capabilities` is
served to the configured uids AND to this process's own uid whatever is
configured: it is the health probe, and the probe is this binary asking
itself, so a deployment whose worker runs as another uid does not fail its
liveness probe for it. The
image ships the socket directory `/run/elitea-extract` owned `10001:10001`
mode `0770`, so a compose named volume mounted there is seeded writable for
the extractor and reachable by the worker's group and nobody else's; in the
chart the directory is an `emptyDir` and the peer check is what holds.

The binary links no HTTP client and no TLS stack at all —
`elitea-engine-sidecar` is taken with `default-features = false` (socket
binder and cgroup reader only), and the service has nowhere to send
anything. Compose runs it with `network_mode: none`; in the chart it shares
the worker pod's network namespace and listens on nothing.

## Limits

Every request's limits are lowered to the process caps (a caller cannot raise
them):

| Variable | Meaning | Default |
| --- | --- | --- |
| `ELITEA_EXTRACTOR_SOCKET` | the socket to serve on | `/run/elitea-extract/extractor.sock` |
| `ELITEA_EXTRACTOR_PEER_UIDS` | the peer uids served `Extract` (comma-separated), read from `SO_PEERCRED`; others are `PERMISSION_DENIED`; `Capabilities` is served to this process's own uid as well | this process's uid |
| `ELITEA_EXTRACTOR_MAX_INPUT_BYTES` | bytes accepted for one document (enforced on the declared length and on the running count) | 50 MiB |
| `ELITEA_EXTRACTOR_MAX_PAGES` | pages read from a paged format; more is `TOO_MANY_PAGES`, never truncated | 1000 |
| `ELITEA_EXTRACTOR_MAX_OUTPUT_BYTES` | text returned for one document | 20 MiB |
| `ELITEA_EXTRACTOR_TIMEOUT_MILLIS` | wall time for one extraction after its bytes arrived; rounded UP to whole seconds for the parser | 120000 |
| `ELITEA_EXTRACTOR_STREAM_IDLE_MILLIS` | the longest wait for a document's next chunk | 30000 |
| `ELITEA_EXTRACTOR_CONCURRENCY` | extractions at once; not below the worker's document concurrency (4) | 4 |
| `ELITEA_EXTRACTOR_SLOT_WAIT_MILLIS` | the longest a further request waits for a slot before `RESOURCE_EXHAUSTED`; separate from the extraction timeout | 600000 |
| `RUST_LOG` | stderr log filter | `info` |

**Capacity, and why queueing is never exhaustion.** The worker keeps no
more calls in flight than the `concurrency` this process advertises
(`Capabilities`), queueing further documents on its own side, so the slot
wait here is reached only by a client that ignores the advertisement. The
slot wait is bounded apart from the extraction timeout, generously, so a
request that does wait is served when a slot frees rather than refused when
an unrelated timer runs out. The worker also asks for an extraction timeout
in the header (its run's, lowered to this process's cap) and derives its
call deadline from that plus the advertised slot wait and idle limit plus a
margin — never a fixed figure.

**Memory.** A request takes its slot BEFORE its document is read, so the
bodies the process holds are bounded by `concurrency` documents at the cap,
however many requests a client has in flight: a queued request holds no
bytes (the socket tests prove it through `held_body_bytes`). The body's
buffer starts at the declared length bounded by the cap and by one 4 MiB
message, growing with the chunks that really arrive. Under a cgroup v2
memory limit the input cap is lowered so that `concurrency` documents at
the cap, expanded six times over, fit in half the limit
(`Config::fit_memory`): a document over that is skipped `too_large` instead
of ending the process with an OOM kill. The fit is
`min(configured, max(budget, 1 MiB))`: it only ever lowers the configured
cap, never raises it. The six counts the one copy of the document the
process holds (the bytes read off the stream move into the parser without a
copy) plus the parser's expansion. The limit under which the DEFAULTS stand
as configured is therefore 50 MiB × 4 × 6 × 2 = 2400 MiB
(`Config::memory_limit_for_caps`), which is why the chart
(`worker.extractor.resources.limits.memory`) and compose (`mem_limit`)
default to 2560 MiB and not a round 2 GiB — under 2 GiB the fit would
quietly lower the cap to ~44 MiB. Raise the limit with `concurrency` or the
cap; the render test asserts the relation. The sidecar also gets a small
memory-backed `/tmp` (chart `worker.extractor.tmpSizeLimit`, 64 MiB;
compose tmpfs) on its read-only root, because xberg may need temp files.

**Timeouts.** The parser (xberg) takes whole seconds, so a request's
timeout is rounded UP to the next second for it
(`elitea_doc_extract::cooperative_timeout`: 1500 ms is two seconds, never
one), and the request waits for its thread for that rounded figure plus a
500 ms grace (`service::outer_timeout`), so the parser's own cooperative
timeout always fires first and an ordinary overrun ends by itself. A thread
that outruns even that cannot be killed; the request is answered `TIMEOUT`
and the thread keeps its slot until it ends. The extraction runs on ONE
dedicated large-stack thread (no `spawn_blocking` wrapper around a second
thread) that owns the slot, the body and its overrun ledger and answers
over a oneshot channel. A request the client CANCELS (or whose deadline the
transport enforces) while its thread runs is accounted exactly like a
timeout, by a guard on the request's future: the thread is an overrun until
it ends. The count of overruns is kept exact by the per-thread ledger
(`OverrunLedger`): the request counts a thread only when it claimed it
first, the thread takes the overrun back only when it found that claim, and
a thread that ended just as its request gave up is not an overrun at all —
its real answer is returned. When every slot is held by an overrun the
process logs `extractor_slots_exhausted` and exits (code 75) after a short
grace, so the container restarts it; the worker records `extract_failed`
for what was in flight and reconnects. `Capabilities` reports `busy` and
`overruns`, and `elitea-doc-extractor healthcheck` fails when
`overruns == concurrency`, so a wedged process also fails its liveness
probe and is restarted even if its own exit did not happen.

**Extractor-side failures.** An xberg `Io`, `Serialization` or `Other`
error is the extractor's own failure (a temp file it could not write, a
serde round-trip), not a statement about the document: it is answered as a
gRPC `INTERNAL` status, which the worker records `extract_failed`, so the
document is retried by the next run instead of being filed `unreadable`.
The page limit is read from xberg's typed `SecurityError::TooManyPages`
source, never from its sentence; the over-limit PDF test runs a real
document through xberg, so an xberg release that dropped the typed source
would fail CI there.

## Deployment

* Image: `ghcr.io/elitea-ng/elitea-doc-extractor` (`Containerfile`, distroless
  `base-nossl-debian13` plus `libgcc_s`, built with `cargo auditable`;
  `docker-bake.hcl`, `publish.yml`, `ci-image-scan.yml`).
* Compose: `deploy/docker-compose.standalone-rust-agent.yml` adds the
  service (2560 MiB `mem_limit`, a 64 MiB tmpfs at `/tmp`), the shared
  `standalone_doc_extractor_sock` volume and `extractor_socket` in
  `deploy/runtime/worker-runtime.rust.json`. The validate worker's document
  (`worker-runtime.rust-validate.json`, derived by
  `deploy/scripts/render-validate-worker-runtime.py`) deliberately drops the
  key: that stack runs no sidecar.
* Helm: `worker.extractor.enabled` (off by default) renders the container in
  the worker pod with a memory-backed `emptyDir` at `worker.extractor.socketDir`
  and writes `extractor_socket` into the worker's `runtime.json`;
  `worker.extractor.peerUids` (empty = the worker's `runAsUser`) is the peer
  check, `probes.timeoutSeconds` (5) covers the probe's own 2 s budget; the
  chart refuses it outside `worker.implementation=rust` with
  `worker.indexingRuntime=rust` (`deploy/helm/tests/render-worker-extractor.sh`).

## Commands

```
elitea-doc-extractor serve         # the default
elitea-doc-extractor healthcheck   # Capabilities over the socket; exit 0 when serving
elitea-doc-extractor --version
```

## Tests

`tests/socket.rs` runs the service over a real Unix socket with fixtures the
test generates (a PDF with an exact cross-reference table, a DOCX, an XLSX
and a PPTX written with the `zip` crate — no committed binaries): each format
gives its text; a corrupt PDF and a corrupt Office container are `CORRUPT`;
an encrypted PDF is `ENCRYPTED`; a three-page PDF under a two-page cap is
`TOO_MANY_PAGES` (process cap and request cap); the input cap on the declared
length, on the counted bytes and against a request asking for more; an
unsupported type without reading the bytes; the output cap; an empty
document; a timeout that holds the only slot and reports exhaustion once; a
peer outside the configured uids refused `PERMISSION_DENIED` on both RPCs;
a queued request served when a slot frees past the extraction timeout, and
refused only when the separate slot wait runs out; and the protocol
refusals (no header, a second header, an empty stream, a stream that goes
quiet). The unit tests in `service.rs` force the timeout race both ways
with hooks (the thread ends first: its answer is used and nothing is
counted; the timeout wins: `TIMEOUT`, counted until the thread ends).
