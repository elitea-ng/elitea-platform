# Browser session agent line protocol, revision 1

The supervisor's `browser` profile (ADR-0049 decisions 1 and 4,
`browser_session.proto`) speaks to the in-pod session agent (#1594) over the
agent's stdin and stdout. There is no socket, port or TLS between them: the
supervisor execs the agent in the session's container, as it execs the
workspace runner, and the exec's pipes are the channel. CDP stays on the
Pod's loopback, between the agent and the engine.

The supervisor side is `services/elitea-worker-rust/src/sandbox/browser_session_agent.rs`
(`SessionAgent`, `LineAgent`, and a test fake that is the reference agent).

## Process

| Command | What it does |
| --- | --- |
| `elitea-browser-session idle` | The container's entrypoint. Starts nothing and waits for a signal. |
| `elitea-browser-session serve` | Run by the supervisor as an exec. Reads commands from stdin and writes replies to stdout. |

- The image installs the agent at `/usr/local/bin/elitea-browser-session`.
- The container runs as uid and gid 10001 with a read-only root
  filesystem. `HOME=/tmp`. `/tmp` (the engine's profile directory) and
  `/dev/shm` are memory-backed.
- The agent writes its own log to stderr only. Stderr never carries a reply,
  and it never carries page content, a token or a URL.
- When stdin closes (end of file), the agent stops the engine and exits 0.
- One `serve` runs per container, for the session's life. A supervisor that
  loses the exec does not attach again. It destroys the session, because a
  session is never reused.

## Framing

- One JSON object per line, UTF-8, ending in `\n`.
- No line is longer than 12 MiB in either direction. The supervisor ends the
  session on a longer line or on a line that is not valid JSON.
- Every object has an `op`. A receiver ignores an unknown `op` it is not
  waiting for. It also ignores unknown fields.
- Binary data (screenshots, frames) is standard base64 with padding.

## Supervisor to agent

| `op` | Fields | Meaning |
| --- | --- | --- |
| `start` | `revision` (1), `engine` (`full` or `lite`), `engine_sandbox` (bool), `max_pages`, `viewport` `{width, height}` (default 1280 by 800), `proxy` (`{url, token}` or `null`), `attended` (bool, optional, default `true`; `false` answers `browser_request_user` with `no_person`) | Always the first line. Start the engine, then answer `ready` or `error`. |
| `call` | `id`, `name`, `args` | One browser tool call from the `libs/rust/browser-tools` contract: a `browser_*` name with an arguments object. Answer exactly one `result` (or `error` with the same `id`). |
| `view` | `on` (bool), `max_fps` (1 to 10) | Start or stop the screencast. `on: true` is sent when the first viewer attaches, and `on: false` when the last one leaves. With no viewer, the screencast does not run. |
| `ack` | `seq` | A viewer showed frame `seq`. |
| `input` | `event` | A driving viewer's input. The supervisor sends it only on a stream opened with control. See the table below. |
| `run_ended` | | The run ended, and the session lingers for up to 10 minutes. No more `call` lines follow. Viewers may still watch. |
| `destroy` | | Stop the engine, write `bye`, and exit. |

### `start`

- `engine_sandbox: false` means the engine runs with `--no-sandbox`. The
  supervisor sends it only when the container runs under gVisor. The agent
  must also refuse it by itself (`error` `isolation_refused`) when it can
  tell it is not under gVisor.
- `proxy` names the engine's only route out:
  - `url` is `http://<ip literal>:<port>`. The container has no resolver,
    so the proxy resolves names.
  - `token` is the signed run token. Send it to the proxy as
    `Proxy-Authorization: Basic <base64(run:<token>)>` on every CONNECT:
    the user is `run` and the password is the token. The proxy does not
    accept `Bearer`.
  - The token exists only in memory. Never write it to argv, the
    environment, a file or a log.
  - `proxy: null` means the engine has no network.
- `attended: false` means nobody can be asked: the run is a schedule or a
  webhook (ADR-0049 decision 7). `browser_request_user` then answers
  `no_person` at once instead of waiting for a person. The supervisor sends
  `false` when the start request sets `unattended`.
- WebRTC, QUIC and DNS-over-HTTPS are off in the engine (ADR-0049
  decision 5).

### `input` events

| `type` | Fields |
| --- | --- |
| `pointer` | `kind` (`move`, `down`, `up`), `x`, `y` (logical pixels of the frame), `button` (0 to 3), `click_count` (up to 3), `modifiers` (CDP bits: Alt 1, Ctrl 2, Meta 4, Shift 8) |
| `wheel` | `x`, `y`, `delta_x`, `delta_y`, `modifiers` |
| `key` | `kind` (`down`, `up`, `char`), `key`, `code`, `text` (at most 32 bytes each), `modifiers` |
| `take_over` | none |
| `hand_back` | `note` (at most 2 KiB) |

The supervisor bounds every field before it sends the line. The control
rules of ADR-0048 decision 4 are the agent's:

- While a person drives (after `take_over`, until `hand_back`), every
  `call` except the watchable ones (`browser_snapshot`,
  `browser_screenshot`) answers with the contract's `user_in_control`
  error.
- Input that arrives while the agent drives pauses the agent.

## Agent to supervisor

| `op` | Fields | Meaning |
| --- | --- | --- |
| `ready` | `engine_version` (for example `chrome-headless-shell/142.0.7444.59`) | The answer to `start`. |
| `result` | `id`, `result` (the contract's result object, failures included as `{"status":"error","code":...}`), optional `image` `{media_type, data}` | The answer to one `call`. `image` is present for `browser_screenshot` only. |
| `frame` | `seq` (increasing), `jpeg`, `width`, `height`, `url`, `title`, `control` (`agent` or `user`) | One screencast frame. Send the latest frame first. Send the next frame only after the supervisor acks this one, and drop the frames in between. At most `max_fps` frames per second. |
| `notice` | `code` (`[a-z_]`, at most 64 bytes) | A non-fatal event, logged by its code only. |
| `error` | `code`, optional `id` | Without `id`: `start` failed, or the session is broken (the supervisor destroys it). With `id`: that call failed outside the contract (the agent could not run it at all). |
| `bye` | none | The agent is exiting. The last line it writes. |

### Timing

| Answer | Limit |
| --- | --- |
| `ready` | Within 60 s of `start`. |
| `result` | Within 90 s of its `call`. The contract's `browser_wait` allows up to 60 s. |

The supervisor fails a call that passes its limit as `browser_agent_failed`,
and the session stays up. An agent whose stdout closes ends the session.

### Errors

Tool failures are `result` lines whose result carries the contract's error
codes (`libs/rust/browser-tools/src/error.rs`), for example `stale_ref`,
`egress_refused` and `user_in_control`. The supervisor never rewrites them.

`error` lines use these codes:

| Code | Meaning |
| --- | --- |
| `engine_start_failed` | The engine did not start. |
| `isolation_refused` | `start` asked for `--no-sandbox` outside gVisor. |
| `protocol` | A line the agent could not parse. |
| `internal` | Any other failure. |

## Not in this protocol

- The grant, the claim and the fence. The supervisor checks them before it
  writes any line.
- The session's lifetime: idle, wall, linger and destroy. The supervisor
  enforces it. The agent only follows `run_ended` and `destroy`.
- Metering. It is the worker's, by engine and wall seconds (ADR-0049
  decision 8).
