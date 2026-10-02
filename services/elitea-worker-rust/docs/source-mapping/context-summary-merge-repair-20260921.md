# Context summary merge repair

This change belongs to gate 4. It changes no database schema or public API.
Current-platform context behavior is mapped in [context summary compatibility](context-summary-compatibility-20260918.md).
Batched compaction is a replatform extension, not a literal legacy port.
The worker retains ADK event summarization and its existing bounded correction flow.
Rust source ownership is `src/agents/context_compaction.rs`.
Regression coverage is `src/agents/context_compaction_tests.rs`.

## Luna batching failure and merge correction

Chat 601 uses a 400,000-token task model with the smaller Luna summarizer.
Its synthetic history contains 18 messages and 1,407,395 bytes.
Execution `731aa9e39f69cbb5fd01ff818ba28df1` resumes after a forced worker restart during compaction.
The worker rejects the oversized summary request locally, then sends smaller requests to Luna.
The run ends with `context_summary_reference`. No completed compaction is accepted.

The merge path has a correction gap in `agents/context_compaction.rs`.
It first validates against intermediate summaries, then checks original records after the correction opportunity.
Generated summary text can satisfy the intermediate check but fail the original-source check.
The candidate change passes original records into merge validation and its existing bounded correction flow.
Original records remain validation data. They are not appended to the smaller merge model request.
The candidate retains rejection when correction still cites generated labels.
Regression tests cover repairable references and persistent invalid references.
The live acceptance below verifies this repair.

The candidate passes all 19 context-compaction tests with the isolated PostgreSQL fixture enabled.
The process-replacement test executes successfully. Rust Clippy passes for library and test targets.
These checks do not prove provider behavior or replacement-worker recovery.
A fresh synthetic chat, 602, supplies the live rerun below.


## Live acceptance

Fresh headed Playwright testing passes in chat 602.
Execution `d4561f80856fdd3027059e416e12c756` restarts the worker during compaction and completes under recovery.
The task model uses a 400,000-token window. The dedicated Luna summarizer uses its configured 272,000-token window.
Estimated task input falls from 353,853 to 40,334 tokens after batching and merge.
The answer preserves CEDAR-731, the corrected teal color, completed archive verification, and the pending handoff note.
The answer remains identical after browser reload. No page errors occur.
The deployed image is `sha256:2de93381b7aed92f15bfb72fd23bac393b0b96d3e90f154a6a24e1106b6f4aa4`.

This is one live compaction with a smaller summarizer and replacement-worker recovery.
It does not prove repeated live compaction, every nested scope, or a real million-token provider route.
