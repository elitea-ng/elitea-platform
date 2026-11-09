---
title: "Output contracts on agents and pipeline LLM nodes (#1500, off until authored)"
issue: 1500
date: 2026-10-30
order: 61
---

ADR-0043 decision 2. No migration. An agent version can store an output
contract in `meta.output_contract`, and a pipeline `llm` node in its
`output_contract` field: a JSON Schema (`{"type":"json_schema","schema":{...}}`),
an RE2 regex (`{"type":"regex","pattern":"..."}`) or a named format
(`{"type":"format","format":"json|markdown|csv"}`).

- **Save validation.** Creating an agent, creating a version or saving a
  version with a contract that is not correct answers HTTP 422
  `OUTPUT_CONTRACT_INVALID` with `reason` and, for a pipeline, `node_id`.
  Limits: schema 32 KiB and 32 levels, local `$ref` only; pattern 1024
  bytes; a structured LLM node cannot have a contract; a pipeline version
  cannot have `meta.output_contract` (`pipeline_contract_on_node`).
- **Behaviour.** The worker validates the final answer, retries one time
  with the validation error, and then ends the agent or node with
  `output_contract.violation`. Main shows `OUTPUT_CONTRACT_VIOLATION`
  (wire code `MODEL_RESPONSE_INVALID`, not retryable). A pipeline does not
  run the next node. An agent or node with a contract does not stream its
  answer. Each validation is a step record (`output`,
  `output_contract_check`); the violation is an `error` step
  `output_contract_violation`.
- **Upgrade order: Main, then the workers** (Argo CD sync wave 0, then wave
  1). **Do not save a contract until every worker runs this release.** For
  a run whose agent, nested agents or pipeline hold a contract, Main sets
  the new `WorkerCommandV1.output_contracts` field (70, `revision` 1). A
  worker of an earlier release refuses the unknown field with
  `INCOMPATIBLE_VERSION`, so the run fails; it never runs without the
  contract. Runs without a contract send nothing new.
- **Response schema strict mode.** A response schema is now sent with
  `strict: true` to OpenAI-compatible providers only when every object lists
  all its properties as required and sets `additionalProperties: false`;
  otherwise it is sent with `strict: false`. The Anthropic adapter sends a
  schema only when it meets the same rules. The worker validates the answer
  in all cases.
- **Agent answers at the output token limit.** For an agent with a contract,
  an answer cut at the output token limit counts as not valid and uses the
  retry. A pipeline LLM node continues the answer as before and validates
  the joined answer.
