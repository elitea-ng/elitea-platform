---
date: 2026-11-23
---
## Toolkits and MCP servers reach private hosts only when an administrator permits them

Agent toolkits, OpenAPI tools, index runs and remote MCP servers now apply the same address check as the web fetch. A tool call cannot reach a loopback, private-network, link-local or cloud metadata address. A refused call fails with `egress_host_not_allowed`, and the run's egress summary lists it with the reason `not_public`.

If your toolkits use an on-premises service on a private network, such as a self-hosted Jira, GitLab or Confluence, a platform administrator lists it in **Configuration › Worker Egress › Private Permit**. You can enter a host name, a `*.domain` or a CIDR block. Metadata and link-local addresses stay refused even when they are listed.
