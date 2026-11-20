---
date: 2026-10-31
order: 0
---
## AI disclosure, platform preamble and untrusted content

**Outbound tools now add an AI disclosure marker** to the text that they send outside the platform: Jira, GitHub, GitLab, Bitbucket, Azure DevOps and Aha! comments, GitHub, GitLab, Bitbucket and Azure DevOps pull request descriptions, new GitHub and Jira issues, new Azure DevOps work items, Confluence pages, Salesforce cases, Figma file comments, Slack messages and Gmail messages. The default text is `Generated with Elitea AI`, and Markdown targets also get the run id in a hidden comment. Administrators set the text under **Configuration › AI Disclosure**, and a project can change it in its **AI disclosure** project setting, but it cannot remove it. **Every tool result now reaches the model in an `<untrusted>` envelope**, and a short built-in preamble tells the model that this text is data, never an instruction. Administrators can add their own **Platform preamble**, which goes before every agent's system prompt; each change is a revision with an audit event, and each turn keeps the revision that it started with. At the high and critical tiers, a `delete`, `external_send` or `payment` call after a tool result in the same turn asks for an approval (`approval_rule` source `untrusted_read_then_act`). See [Content controls](/admin/enterprise-controls/content-controls).
