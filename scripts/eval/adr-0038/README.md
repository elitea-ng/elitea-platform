# ADR-0038 phase 0 evaluation

This harness measures two things that ADR-0038 needs before the build:

1. A single agent run compared with a prototype **verified loop**
   (decisions 6–8).
2. Thinking **on**, **off** and the **`auto`** policy (decision 4) on
   routine tool steps.

It records quality (pass rate against the real check), cost (prompt and
completion tokens, with the reasoning tokens apart) and wall time.

Tracking issue: elitea-ng/elitea-platform#1452.

This is a measurement tool, not product code. The prototype loop lives here
and nowhere else.

## Requirements

- Node 22 or later. No npm packages. The harness uses `node:http` and
  `node:https`, `node:child_process`, `node:fs` and `node:test`.
- The client posts with `node:http`, not the global `fetch`. `fetch`
  (undici) drops a request when the response headers take longer than
  300 s. A long reply that is not streamed, on a loaded server, takes that
  long: the reply headers come only when the whole generation is done. The
  client sets its own request timeout (30 min). A timeout is final and is
  not retried.
- `go` on `PATH` for the Go tasks.
- macOS: `sandbox-exec` (built in). On Linux the harness uses
  `unshare -rn` when user namespaces are available. Without either,
  commands run with no network block, and the results file says
  `sandbox: none`.

## Run it

```bash
cd scripts/eval/adr-0038

# Unit tests of the harness itself (a scripted fake model, no server).
node --test "test/*.test.mjs"

# Check that every task is well formed: the start state fails the real
# check, and the reference solution passes it (and the held-out tests).
node run.mjs --selftest

# Full run: 5 variants, 2 repeats, against the local Qwen. Set its endpoint first.
export ELITEA_EVAL_QWEN_BASE_URL=http://<vllm-host>:8000/v1
node run.mjs --provider qwen --repeat 2

# A subset: only the extraction tasks, variants b and c.
node run.mjs --provider qwen --tasks '^extract-' --variants b,c
```

Options:

| Option | Default | Meaning |
|---|---|---|
| `--provider` | `qwen` | An entry of `providers.json`. |
| `--variants` | `a,b,c,d` | Which variants to run (see below). |
| `--repeat` | `1` | Repeats of each task and variant, to see the noise. |
| `--tasks` | all | A regular expression on the task id or group. |
| `--concurrency` | the provider's `concurrency` | Parallel runs. |
| `--max-steps` | `40` | Step limit of a single run. |
| `--worker-steps` | `20` | Step limit of one loop worker. |
| `--max-rounds` | `8` | Round limit of the loop. |
| `--out` | `results/<provider>-<date>` | Output path without the extension. |

The run appends each finished run to `<out>.jsonl`. When you start the same
command again, it skips the runs that are already in that file. At the end
it writes `<out>.json` (all records and the run settings) and `<out>.md`
(the summary tables). `node summarize.mjs <out>.json` prints the tables
again. The analysis at the end of a committed `.md` is written by hand. A
new run to the same `--out` overwrites it, so copy it first.

Current results: `results/qwen-2026-11-04-r2.md`. The first run, `results/qwen-2026-11-04.md`, is kept for the comparison.

## Variants

| Key | Variant | Thinking |
|---|---|---|
| a | single run | on for every step |
| b | single run | off for every step |
| c | single run | `auto`: on for the first step and after a tool error or a failed command; off for routine follow-up steps |
| d | verified loop | manager on; workers `auto`; summary and finish calls off; reviewer on |
| e | control: single run told the check | `auto`, as (c). The prompt also gets the check description that a loop worker sees. It separates the gain of the loop from the gain of telling the agent the check. Not in the default set: run it with `--variants e`. |
| f | single run, `auto-exit` | `auto`, and also on after a `run_command` that exits non-zero (a failing test run, but also a `grep` with no match). Not in the default set: run it with `--variants f`. |

For the `auto` policy, a tool result is a failure only when the tool itself
failed: bad arguments, a refused path, an unknown tool, or a `run_command`
that timed out, that the sandbox refused, or that could not start. The exit
code of a command goes to the model as data. A non-zero exit, such as a
failing test run or a `grep` with no match, does not turn thinking on. The ADR rule "on for the final answer with an output schema" does not
apply here: the extraction tasks write a file through a tool, and the policy
cannot know which step is the last one.

## Add a provider

Add one entry to `providers.json`. Nothing else changes.

```json
"gpt": {
  "base_url": "https://api.openai.com/v1",
  "key_env": "OPENAI_API_KEY",
  "model": "<model id>",
  "thinking": { "kind": "effort", "on": "medium", "off": "none" },
  "temperature": null,
  "max_tokens": 12288,
  "concurrency": 4
}
```

| Field | Meaning |
|---|---|
| `base_url` | An OpenAI-compatible endpoint. The harness posts to `<base_url>/chat/completions`. |
| `base_url_env` | In place of `base_url`: the environment variable that holds the endpoint. Use it for a private endpoint, so that its address stays out of the repository. The `qwen` entry reads `ELITEA_EVAL_QWEN_BASE_URL`. |
| `key_env` | The environment variable that holds the API key, or `null` for no key. |
| `model` | The model id. |
| `thinking.kind` | The control kind of ADR-0038 decision 2. See the table below. |
| `temperature` | A number, or `null` when the provider refuses a temperature (for example Anthropic with thinking on). |
| `max_tokens` | The completion cap per call. `max_tokens_field` renames the field (`max_completion_tokens` for OpenAI reasoning models). |
| `concurrency` | Default parallel runs for this provider. |

| `thinking.kind` | On | Off |
|---|---|---|
| `toggle` | `chat_template_kwargs.enable_thinking: true` | `false` |
| `effort` | `reasoning_effort: <on>` (default `medium`) | `reasoning_effort: <off>` (default `none`) |
| `budget`, `style: anthropic` | `thinking: {type: enabled, budget_tokens: <budget>}` | the field is left out |
| `budget`, `style: gemini` | `extra_body.google.thinking_config.thinking_budget: <budget>` | budget `0` |

The `gpt`, `claude` and `gemini` entries are templates. Nobody has run them
yet. Set the model id and check the thinking form against the provider
before the first run.

The harness reads `usage.completion_tokens_details.reasoning_tokens` for the
reasoning tokens. A provider that does not report them shows `0`.

## The verified loop prototype

`lib/loop.mjs` follows ADR-0038 closely enough to be a fair test:

1. The baseline: the harness runs a strong verifier once before any work.
   The manager gets the result. This costs no model tokens.
2. **Plan.** The manager (thinking on) writes the acceptance criteria and a
   task list into the ledger.
3. **Round.** A worker runs one task in a fresh context. It sees the
   ledger (goal, criteria, check, task list, earlier verdicts, the last
   worker's notes) and the workspace. It does not see earlier transcripts.
   Its report becomes the notes. A worker that hits the step limit or the
   output cap gets a summary call (thinking off).
4. **Verify.** The task's verifier runs on the workspace.
5. **Review.** The manager (thinking on) reads the verdict and says `done`
   or gives the next task. Its prompt ends with a status line: rounds used,
   tokens spent, verdicts so far.
6. **Finish.** One call (thinking off) writes the final answer.

The hard rules are code:

- A `done` while a strong verifier fails becomes "continue".
- Regression guard: when a passing result exists, a failing round is rolled
  back to it. Without a pass, the best failing result by test score is kept.
- Stop on `done` with a pass, at the round limit (8), or when the manager
  gives the same task twice in a row.
- A strong pass runs the verifier once more. A pass that does not repeat
  counts as flaky.
- The check files are hashed at the start. A pass on changed check files
  counts as "verifier changed", not as a pass.

The prototype leaves out the money envelope, steering, ideation and contract
changes. ADR-0038 needs them in the product, but they do not change what
this evaluation measures.

## Verifiers and the real check

| Group | Verifier in the loop | Real check after the run |
|---|---|---|
| coding | `command`: the public test command; strong | The harness restores the original test files, runs the public tests, then copies in the held-out tests (if any) and runs them. |
| extraction | `schema`: `output.json` validates against `schema.json`; strong for shape only | Schema valid **and** every expected value matches (`expected.json`). |
| doc | `review`: a separate model call against the request and the acceptance criteria; weak | A fixed mechanical rubric (`rubric.json`): required sections, facts and exclusions. |

A doc run of the loop is **model-reviewed**, never verified. The pass rate
in the tables for doc tasks is the rubric result, not the reviewer's
verdict.

The real check never trusts the workspace's copy of a test file, the schema
or the rubric.

## Task format

```
tasks/<id>/
  task.json      id, group, title, prompt, verifier, final (hidden command)
  files/         the start workspace
  hidden/        held-out tests, copied in only for the real check
  solution/      a reference solution (selftest only; never copied into a run)
  expected.json  extraction: expected values
  rubric.json    doc: rubric checks
```

## Tasks

26 tasks: 14 coding, 8 extraction, 4 doc.

| Task | Group | Verifier | Held-out | Title |
|---|---|---|---|---|
| `code-business-days` | coding | command: `node --test "test/*.test.mjs"` |  | Implement business-day arithmetic with holidays (UTC) |
| `code-csv-quotes` | coding | command: `node --test "test/*.test.mjs"` | yes | Fix RFC 4180 edge cases in the CSV parser and writer |
| `code-event-emitter` | coding | command: `node --test "test/*.test.mjs"` |  | Fix once/off-during-emit semantics in the event emitter |
| `code-go-cron` | coding | command: `go test ./...` |  | Implement a 5-field cron expression parser and Next() (Go) |
| `code-go-intervals` | coding | command: `go test ./...` | yes | Implement closed integer interval merge/insert/remove (Go) |
| `code-go-roman` | coding | command: `go test ./...` | yes | Fix Roman numeral conversion and strict validation (Go) |
| `code-go-wrap` | coding | command: `go test ./...` |  | Fix the greedy word wrapper (runes, paragraphs, long words) (Go) |
| `code-json-pointer` | coding | command: `node --test "test/*.test.mjs"` | yes | Implement RFC 6901 JSON Pointer get/set/remove |
| `code-lru-ttl` | coding | command: `node --test "test/*.test.mjs"` |  | Fix recency and expiry bugs in the LRU cache with TTL |
| `code-money-cents` | coding | command: `node --test "test/*.test.mjs"` |  | Switch the checkout modules from float dollars to integer cents |
| `code-semver-prerelease` | coding | command: `node --test "test/*.test.mjs"` |  | Fix SemVer 2.0.0 precedence and validation |
| `code-template-sections` | coding | command: `node --test "test/*.test.mjs"` | yes | Implement a mustache-style template renderer |
| `code-token-bucket` | coding | command: `node --test "test/*.test.mjs"` |  | Fix token-bucket refill and wait-time math |
| `code-word-freq` | coding | command: `node --test "test/*.test.mjs"` |  | Make word frequency counting Unicode-aware with stable tie order |
| `doc-api-reference` | doc | review |  | Write an API reference for a small JavaScript module |
| `doc-decision-record` | doc | review |  | Write an architecture decision record from a meeting transcript |
| `doc-release-notes` | doc | review |  | Write user-facing release notes from a git log |
| `doc-runbook` | doc | review |  | Write an on-call runbook from a service config and incident notes |
| `extract-changelog` | extraction | schema |  | Count changelog entries per released version and category |
| `extract-flight-itinerary` | extraction | schema |  | Extract a multi-segment flight itinerary with UTC times and a schedule change |
| `extract-incident-timeline` | extraction | schema |  | Merge an incident timeline written in several time zones into UTC |
| `extract-invoice` | extraction | schema |  | Extract a two-page German-format invoice into structured JSON |
| `extract-job-posting` | extraction | schema |  | Extract salary, hours and remote policy from a job posting |
| `extract-lease` | extraction | schema |  | Extract lease terms, applying addenda that override the main contract |
| `extract-meeting-actions` | extraction | schema |  | Extract action items, owners and due dates from meeting notes |
| `extract-recipe` | extraction | schema |  | Extract and scale a recipe with fractions and mixed units |

## Files

| Path | Purpose |
|---|---|
| `run.mjs` | The runner and `--selftest`. |
| `summarize.mjs` | The markdown summary of a results file. |
| `providers.json` | Provider entries. |
| `lib/provider.mjs` | The chat client and the thinking control per kind. |
| `lib/sandbox.mjs` | Workspaces, the four worker tools, the command sandbox. |
| `lib/agent.mjs` | One agent run and the thinking policy. |
| `lib/loop.mjs` | The prototype verified loop. |
| `lib/tasks.mjs` | Task loading, the verifiers and the real check. |
| `lib/schema.mjs` | The JSON Schema subset, expected-value checks, the rubric. |
| `results/` | Results files. |
