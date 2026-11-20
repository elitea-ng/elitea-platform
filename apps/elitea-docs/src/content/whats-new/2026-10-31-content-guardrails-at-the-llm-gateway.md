---
date: 2026-10-31
order: 2
---
## Content guardrails at the LLM gateway

A new governance entry type, **Content guardrail**, checks model content at the LLM gateway. An entry runs on the request before the provider call (`input`) or on the answer (`output`), for a platform, project, model or agent scope. Its detectors find bounded RE2 patterns and terms, the platform secret rules (for example AWS keys, GitHub tokens and private keys), or the findings of a classifier that you operate, through a documented HTTP adapter contract. Each detector blocks, masks or flags. **A blocked request gets HTTP 400 `guardrail_blocked`; a blocked stream ends with a typed error event.** A streamed mask holds back a short window of text, so that the client never gets a matched value, also when the value is split across chunks. Each check is recorded as a verdict without the matched text, and the run steps API returns the verdicts of a run. Nothing changes until you create an entry. See [Guardrail pipeline](/admin/enterprise-controls/content-controls#guardrail-pipeline).
