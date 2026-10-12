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
`extract_failed`. `Capabilities` lists the media types, the caps and the
capacity (`concurrency`, `slot_wait_millis`, `stream_idle_millis`), and is
the health probe; the worker's client reads it once before its first
document (again after any failure) and shapes itself to it, below.

The worker side is `services/elitea-worker-rust/src/transport/extractor_client.rs`;
it is used by `src/indexing/pipeline.rs` for every media type
`elitea_doc_extract::is_document_type` names when `extractor_socket` is set
in the runtime document, and such documents stay `unsupported_type` when it
is not. The worker maps the skips onto its summary labels: `too_large`
(input or output), `too_many_pages`, `encrypted`, `unsupported_type`,
`unreadable` (corrupt), `extract_timeout`, `empty`; plus `extract_failed`
for the status case.

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
every shipped deployment) are served — any other peer gets
`PERMISSION_DENIED` on both RPCs before a byte of a document is read. The
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
| `ELITEA_EXTRACTOR_PEER_UIDS` | the peer uids served (comma-separated), read from `SO_PEERCRED`; others are `PERMISSION_DENIED` | this process's uid |
| `ELITEA_EXTRACTOR_MAX_INPUT_BYTES` | bytes accepted for one document (enforced on the declared length and on the running count) | 50 MiB |
| `ELITEA_EXTRACTOR_MAX_PAGES` | pages read from a paged format; more is `TOO_MANY_PAGES`, never truncated | 1000 |
| `ELITEA_EXTRACTOR_MAX_OUTPUT_BYTES` | text returned for one document | 20 MiB |
| `ELITEA_EXTRACTOR_TIMEOUT_MILLIS` | wall time for one extraction after its bytes arrived | 120000 |
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

**Memory.** Under a cgroup v2 memory limit the input cap is lowered so that
`concurrency` documents at the cap, expanded six times over, fit in half the
limit (`Config::fit_memory`): a document over that is skipped `too_large`
instead of ending the process with an OOM kill. The fit is
`min(configured, max(budget, 1 MiB))`: it only ever lowers the configured
cap, never raises it. The six counts the one copy of the document the
process holds (the bytes read off the stream move into the parser without a
copy) plus the parser's expansion. Set the container's memory limit
(`worker.extractor.resources.limits.memory`, compose `mem_limit`) and the
cap follows.

**Timeouts.** An extraction thread that outruns its timeout cannot be
killed; the request is answered `TIMEOUT` and the thread keeps its slot
until it ends. The count of such overruns is kept exact by a per-thread
ledger (`OverrunLedger`): the timeout counts a thread only when it claimed
it first, the thread takes the overrun back only when it found that claim,
and a thread that ended just as its timeout fired is not an overrun at all
— its real answer is returned. When every slot is held by an overrun the
process logs `extractor_slots_exhausted` and exits (code 75) after a short
grace, so the container restarts it; the worker records `extract_failed`
for what was in flight and reconnects.

## Deployment

* Image: `ghcr.io/elitea-ng/elitea-doc-extractor` (`Containerfile`, distroless
  `base-nossl-debian13` plus `libgcc_s`, built with `cargo auditable`;
  `docker-bake.hcl`, `publish.yml`, `ci-image-scan.yml`).
* Compose: `deploy/docker-compose.standalone-rust-agent.yml` adds the
  service, the shared `standalone_doc_extractor_sock` volume and
  `extractor_socket` in `deploy/runtime/worker-runtime.rust.json`.
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
