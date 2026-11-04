-- 0225_model_reasoning_control.sql — how a model's thinking is controlled,
-- and the reasoning tokens a request used (ADR-0038 decisions 3 and 5;
-- issue #1452).
--
-- ## elitea_runtime.model_reasoning_defaults
--
-- A reasoning model takes one of five control forms (llm-wire
-- `thinking::ReasoningControl`):
--
--   effort     reasoning_effort: none | low | medium | high
--   budget     a thinking budget, or adaptive thinking where the model has it
--   toggle     chat_template_kwargs.enable_thinking (a Qwen-family model on
--              vLLM or SGLang)
--   fixed_on   the model always thinks; nothing is sent
--   fixed_off  the model cannot think; nothing is sent
--
-- An llm_model row may declare its own form in `data.reasoning_control`,
-- and whether `auto` keeps thinking on routine tool steps in
-- `data.reasoning_tool_steps`. The row's own keys always win. A row that
-- declares nothing, and that says `supports_reasoning: true`, takes the first
-- default here whose pattern matches its provider model name. A row that
-- matches nothing keeps the form its caller used before this file: budget on
-- the native Anthropic wire, effort on every other wire.
--
-- name_pattern is a case-insensitive regular expression in the syntax common
-- to PostgreSQL and Go RE2 (no look-around, no back-reference). Main matches
-- it against the whole `data.name`, a `provider/` prefix included, so a
-- pattern that must match the start of the model id writes `(^|/)`.
--
-- tool_steps is true where a provider refuses, or degrades, when the thinking
-- form changes inside one tool loop: Claude and Gemini carry the thinking
-- block or signature from step to step. Every other family starts with false,
-- the conservative choice of ADR-0038 decision 4: `auto` then does not think
-- on a routine step after a tool result.
--
-- A family is seeded only where its off form works. `auto` sends the off
-- form on a routine step and on a summary, and a model that refuses it (an
-- OpenAI o-series model refuses reasoning_effort none) would fail there. A
-- model with no row and no own key keeps `auto` on at the default level on
-- every step.
--
-- An operator changes the defaults with SQL; no API writes this table. A
-- later release may add rows; it never rewrites a row an operator changed,
-- because every seed below is ON CONFLICT DO NOTHING.
--
-- ## gateway.llm_request_logs.reasoning_tokens
--
-- The completion tokens the provider reported as reasoning
-- (completion_tokens_details.reasoning_tokens, the top-level reasoning_tokens
-- of SGLang, or output_tokens_details.reasoning_tokens of the Responses API).
-- They are a SUBSET of completion_tokens, not an addition to it. Zero is the
-- honest value for a provider that reports no count and for a row written
-- before this file. No backfill.
--
-- The gateway writes the column while the database has it and falls back to
-- the list without it on 42703, so a gateway that rolls out before this file
-- loses the count and no row.
--
-- No permission. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

CREATE TABLE IF NOT EXISTS elitea_runtime.model_reasoning_defaults (
    name_pattern      text PRIMARY KEY,
    reasoning_control text NOT NULL
        CONSTRAINT model_reasoning_defaults_control_check
        CHECK (reasoning_control IN ('effort', 'budget', 'toggle', 'fixed_on', 'fixed_off')),
    tool_steps        boolean NOT NULL DEFAULT FALSE,
    -- The lower number is tried first. Two patterns that match one name are
    -- ordered by priority, then by name_pattern.
    priority          integer NOT NULL DEFAULT 100,
    CONSTRAINT model_reasoning_defaults_pattern_check
        CHECK (length(name_pattern) BETWEEN 1 AND 256)
);

COMMENT ON TABLE elitea_runtime.model_reasoning_defaults IS
    'Family defaults of the reasoning control form of an llm_model (ADR-0038 decision 3). A model''s own data.reasoning_control and data.reasoning_tool_steps win.';
COMMENT ON COLUMN elitea_runtime.model_reasoning_defaults.name_pattern IS
    'Case-insensitive regular expression (PostgreSQL and Go RE2 common syntax) matched against the llm_model data.name.';
COMMENT ON COLUMN elitea_runtime.model_reasoning_defaults.reasoning_control IS
    'effort | budget | toggle | fixed_on | fixed_off.';
COMMENT ON COLUMN elitea_runtime.model_reasoning_defaults.tool_steps IS
    'True: reasoning_effort auto keeps thinking on routine tool steps for this family.';
COMMENT ON COLUMN elitea_runtime.model_reasoning_defaults.priority IS
    'Lower is tried first; ties are ordered by name_pattern.';

INSERT INTO elitea_runtime.model_reasoning_defaults (name_pattern, reasoning_control, tool_steps, priority) VALUES
    -- Models that always think, before the family rows that would match them.
    ('qwen[^/]*-thinking',            'fixed_on', FALSE, 10),
    ('(^|/)qwq',                      'fixed_on', FALSE, 10),
    ('deepseek-(r1|reasoner)',        'fixed_on', FALSE, 10),
    ('(^|/)gemini-[0-9.]+-pro',       'fixed_on', TRUE,  10),
    -- Qwen3 and later on vLLM or SGLang switch thinking per request.
    ('(^|/)qwen',                     'toggle',   FALSE, 50),
    -- OpenAI models that take reasoning_effort none (gpt-5.1 and later).
    -- gpt-5, gpt-5-mini and the o-series refuse none, so they have no row:
    -- the worker then never sends the off form for auto.
    ('(^|/)gpt-5\.[1-9]',            'effort',   FALSE, 50),
    -- Claude and Gemini 2.5 Flash carry the thinking state across a tool
    -- loop, and both take thinking off.
    ('claude',                        'budget',   TRUE,  50),
    ('(^|/)gemini-2\.5-flash',       'budget',   TRUE,  60)
ON CONFLICT (name_pattern) DO NOTHING;

ALTER TABLE gateway.llm_request_logs
    ADD COLUMN IF NOT EXISTS reasoning_tokens BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN gateway.llm_request_logs.reasoning_tokens IS
    'Completion tokens the provider reported as reasoning (ADR-0038 decision 5). A subset of completion_tokens; 0 when the provider reports none.';
