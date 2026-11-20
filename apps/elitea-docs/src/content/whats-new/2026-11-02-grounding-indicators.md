---
date: 2026-11-02
order: 2
---
## Grounding indicators

An agent can now cite its sources. In the agent editor, open **Advanced** and select **Require citations**. The worker numbers each tool result of the current turn, and the model cites the results as `[1]`, `[2]`. Under the answer, the chat shows **N of M sources cited** and **K statements without a citation**. Click the second part to see those statements. The message keeps the indicator as `meta.grounding`, and each run records a `grounding_check` step without the answer text. There is still no confidence score: a cited source does not prove the statement. An answer without tool results has no indicator. **Upgrade every worker before you turn on citations.** See [Grounding indicators](/admin/enterprise-controls/content-controls#grounding-indicators).
