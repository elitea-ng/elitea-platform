---
date: 2026-10-31
order: 1
---
## Output contracts

Agents and pipeline LLM nodes can now have an output contract: a JSON Schema, a regular expression, or a named format (`json`, `markdown` or `csv`). Set it under **Advanced › Output contract** in the agent editor, or in the settings of an LLM node. The worker sends a JSON Schema to providers that support a response schema, and validates each final answer. If an answer is not valid, the model gets one more try with the validation error. If the second answer is also not valid, the run stops with `OUTPUT_CONTRACT_VIOLATION`, and **a pipeline does not run the next node**. A save with a contract that is not correct fails with `OUTPUT_CONTRACT_INVALID`. Each check is a run step record. **Upgrade every worker before you save a contract.** The platform does not give a confidence score; the same page tells why, and how to get a confidence field from the agent. See [Output contracts](/admin/enterprise-controls/content-controls#output-contracts).
