---
date: 2026-10-30
order: 1
---
## Connector approval, credential inventory and permission trimming

A new catalogue mode, `approved_only`, makes toolkit types and MCP servers usable only after a connector request is approved by someone other than the requester; approval grants the type and adds its hosts to the egress allowlist, and a custom MCP server's tools are pinned. The default stays `open`, and the governed profile forces `approved_only`. **Admin › Credential inventory** lists every credential an agent can use with its last use, and flags credentials unused for 90 days (revoking stale tokens is opt-in). The **Permission trimming** report compares what each agent is granted with what it used, for one-click audited removal, and notifies owners weekly. See [Least privilege and trust](/admin/enterprise-controls/least-privilege-and-trust).
