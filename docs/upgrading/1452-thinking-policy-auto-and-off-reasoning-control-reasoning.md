---
title: "Thinking policy: auto and off, reasoning control, reasoning tokens (#1452, ADR-0038 decisions 1 to 5)"
issue: 1452
date: 2026-11-04
adr: ADR-0038
order: 70
---

Upgrade the workers (`elitea-worker-rust`) before Main. A
worker of an earlier release refuses `reasoning_effort` `auto` and `off`
with `invalid_profile`, and Main now writes `auto` on a new agent and on a
chat with no agent. Main also sends the model's reasoning control in a new
proto field (`AgentExecutionInputV1.model_reasoning`, field 70, and
`SummaryModelSnapshotV1.model_reasoning`, field 16). An earlier worker
refuses an input that carries these fields, which is one more reason to
upgrade the workers first.

New shared migration `0225_model_reasoning_control`:

- It creates `elitea_runtime.model_reasoning_defaults`, the family
  defaults of the reasoning control form. It seeds Qwen (`toggle`),
  Qwen thinking models, QwQ, DeepSeek R1 and Gemini Pro (`fixed_on`),
  gpt-5.1 and later (`effort`), GPT-5.6 (`effort` with
  `off_with_tools`), and Claude and Gemini 2.5 Flash (`budget`). The seeds are `ON CONFLICT DO NOTHING`: a later run keeps a
  row you changed. No API writes this table. Change it with SQL.
- It adds `reasoning_tokens` (default 0) to `gateway.llm_request_logs`.
  There is no backfill. The gateway writes the column when it exists. A
  gateway that rolls out before the migration writes rows without it.
- It adds `reasoning_tokens` (default 0) to `gateway.llm_usage_events`.
  The scheduler's write-back consumer and the gateway's outage path write
  it only after they find the column in `information_schema`. A scheduler
  or gateway that runs before the migration loses the count, not the row.

A model's own `data.reasoning_control` and `data.reasoning_tool_steps` in
the catalogue win over the family defaults. A default applies only to a
model with `supports_reasoning: true`. A model with no form keeps the form
of the earlier release: a thinking budget on the native Anthropic wire, a
reasoning effort on every other wire. For such a model, `auto` thinks at
the medium level on every step and never sends the off form.

A Claude model without adaptive thinking, on the native Anthropic wire,
now gets a `high` thinking budget of 8 192 tokens. The earlier release
sent 9 092, a typo with no recorded reason. Low (2 048) and medium
(4 096) are unchanged.

A model's `data.reasoning_off_with_tools` (or the family column
`off_with_tools`) says the model refuses thinking beside tools. On a
request that carries tools, `auto`, or no setting, then sends the off form
on every step, planning included. A request without tools still thinks,
and an explicit level is sent as chosen. This replaces the worker's
built-in GPT-5.6 name rule. The worker reads it from
`ModelReasoningV1.off_with_tools` (field 3), and the engines from
`llm_settings.reasoning_off_with_tools`.

On a `budget` model, `auto` keeps one thinking state, medium, for every
step of a tool loop: the loop never turns thinking off and then on again.
An llm_model save refuses `reasoning_control: budget` with
`reasoning_tool_steps: false`. An output continuation under `auto` sends
off on a `toggle` model, keeps the loop's state on a `budget` model, and
sends `low` on an `effort` model.

Existing agents keep their stored `reasoning_effort`. No migration rewrites
them, and a stored `none` reads as `off`. A save now refuses `off` on a
model that always thinks and a level on a model that cannot think, with
HTTP 400. An llm_model save refuses a `reasoning_control` that is not
`effort`, `budget`, `toggle`, `fixed_on` or `fixed_off`, and a form other
than `fixed_off` on a model with `supports_reasoning: false`.

One rule for a setting the model cannot honour, on every surface. At save
(an agent version, a toolkit's settings, a chat participant), a level on a
model that cannot think, a model with `supports_reasoning: false`
included, and `off` on a model that always thinks, are refused with HTTP
400. At run (agent admission, a toolkit's engine call), a stored value of
that kind, or one that is not canonical, runs as `auto` and Main logs it at
warn with the model and the source. A run is never refused for it: the
earlier release refused such an agent at admission and an engine call at
the callback. A model that cannot think is frozen as `fixed_off` for the
worker, so `auto` sends nothing to it.

`predict_llm` and the AI drafts translate `reasoning_effort` with the
model's control, as agents do. They refuse a value that is not `auto`,
`off`, `low`, `medium` or `high` (`none` reads as `off`), and a value the
model cannot honour, with HTTP 400. They no longer pass the value through
raw. The DeepWiki and Inventory engines get the model's control, so `auto`
there turns thinking off on routine tool steps.

Client contract 1.35:

- `reasoning_effort` takes `auto` and `off`.
- The model catalogue item returns `reasoning_control`,
  `reasoning_tool_steps` and `reasoning_off_with_tools`.
- The run analytics figures and the local child turn usage return
  `reasoning_tokens`. So do the budget usage totals, per day and per
  model. These tokens are a part of `completion_tokens`, not an addition
  to it.
