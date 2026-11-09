---
title: "AI disclosure, platform preamble and untrusted content (#1500)"
issue: 1500
date: 2026-10-30
order: 60
---

ADR-0043 decisions 3 and 4. Shared migration **0216** adds
`elitea_runtime.platform_preamble_revisions` (append-only: one row per
change of the platform preamble, with its SHA-256, author and the id of its
security audit event). Worker commands gain field **71**
(`WorkerCommandV1.content_controls`), and the agent input bundle gains field
**67** (`AgentExecutionInputV1.platform_preamble`).

- **Rollout order.** Roll out elitea-main first, then the workers. Main sends
  field 71 only after an administrator saves **Configuration › AI
  Disclosure**, **Configuration › Platform Preamble**, or the new key
  `untrusted_read_then_act_tiers` of **Configuration › Approval Policy**. A
  worker of an earlier release refuses a command with field 71. Save these
  sections only after every worker runs this release. An approval policy that
  an earlier release saved does not have the new key, so it does not send
  field 71.
- **The preamble text is not on the command bus.** Admission freezes the
  newest preamble revision (text, revision, SHA-256) into the turn's input
  bundle (field 67), which the worker fetches over gRPC. Field 71 carries
  only the revision and the SHA-256; the worker refuses a turn when the text
  does not match its SHA-256 or the command names another revision. Field 67
  is present only after the first save of **Configuration › Platform
  Preamble**; a worker of an earlier release refuses an input bundle with it.
  Field 71 still carries the marker text: it is at most 200 bytes, control
  characters are replaced, it never goes to a model, and it is posted to
  external systems by design.
- **Behaviour change: outbound text gets a marker.** A worker of this
  release adds `Generated with Elitea AI` (a blank line, then the text) to
  the text of the tools in `libs/proto/contracts/tool-disclosure-v1.json`,
  also before any section is saved. Markdown targets also get
  `<!-- elitea-run-id: … -->`. To change the text, save **Configuration › AI
  Disclosure**. Automations that compare the text of comments that agents
  wrote must allow for the marker. Some `write` tools also get the marker
  (their category, so their approval, does not change): GitHub
  `create_pull_request`, `create_issue`, `update_issue`,
  `create_issue_on_project`, `update_issue_on_project`; GitLab and GitLab Org
  `create_pull_request`; Azure DevOps Repos `create_pull_request`; Bitbucket
  `close_pull_request`; Confluence `create_page`; Salesforce `create_case`,
  `update_case`; Carrier `create_ticket`; TestIO `confirm_bug_fix`. Tools
  that take their text in a JSON document get the marker in the field that a
  JSON pointer names: Jira `create_issue` (`/fields/description`; an ADF
  description gets a last paragraph), Bitbucket `create_pull_request`
  (`/description`) and Azure DevOps Boards `create_work_item`
  (`/fields/System.Description`, as an HTML paragraph). An absent field gets
  the marker only. On Jira API v3, `create_issue` now sends a string
  description as an ADF document (Jira refused it before).
- **Behaviour change: tool results are wrapped.** Each tool result now
  reaches the model as `<untrusted source="TOOL">…</untrusted>`, and a
  built-in preamble (approximately 30 tokens) goes before every system
  prompt. Each tool result costs approximately 10 to 15 more tokens. Tools
  that read the model's input, for example an evaluation that compares it
  with a raw tool result, see the envelope.
- **Behaviour change: read untrusted, then act.** At the `high` and
  `critical` tiers, a `delete`, `external_send` or `payment` call that comes
  after a tool result in the same turn asks for an approval. Under the default
  approval policy these calls already ask at these tiers, so nothing changes.
  Before the approval policy is saved, a worker of this release applies the
  rule only when the command carries the approval policy (field 68). To turn
  the rule off, save an empty **Tiers With the Read Untrusted, Then Act
  Rule**.
- **Project setting.** A new configuration type `ai_disclosure` (section
  `project_settings`, gate `models.project_settings.edit`) holds a
  project's marker text and opt-outs. Opt-outs take effect only for the
  toolkit types that the platform section allows.
- **Evidence.** A preamble change writes
  `ai.elitea.security.v1.config.changed` (action
  `config.platform_preamble.revised`) to the security audit outbox.
