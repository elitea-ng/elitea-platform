---
date: 2026-10-30
order: 5
---
## Approval policy by risk tier and tool category

Every built-in tool now has a category: `read`, `write`, `delete`, `external_send`, `payment` or `code_execution`. Under **Configuration › Approval Policy**, administrators select for each risk tier the categories whose tools pause for a human approval. The default asks for `delete`, `external_send` and `payment` tools at the high and critical tiers. The policy adds to the sensitive action tools, and agent authors cannot remove an approval. Each pause names the rule that caused it (`approval_rule`). The same section can require a `hitl` node before every `external_send` or `payment` tool node in pipelines at selected tiers; **a pipeline that breaks the rule does not start** (`PIPELINE_HITL_REQUIRED`). The policy applies after the section is saved; save it after every worker runs this release. See [Tool categories and approval policy](/admin/enterprise-controls/runtime-safety#tool-categories-and-approval-policy).
