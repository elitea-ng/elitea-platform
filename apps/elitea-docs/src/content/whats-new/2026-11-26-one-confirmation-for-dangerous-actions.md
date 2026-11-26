---
date: 2026-11-26
---
## One confirmation, with a reason, for dangerous admin actions

In the admin console, every dangerous action now asks in the same dialog: deleting users, projects, roles, platform models and providers, identity providers and SCIM clients; changing roles, trust, guardrails or retention; resetting branding, revoking a credential, inviting in bulk and stopping the whole platform. The dialog says whether the action is dangerous or critical, what it changes and how many projects or people it reaches, asks for a reason code and an optional reason, and arms only when you type the target's name. The reason is recorded on the audit event. See [Dangerous actions ask for a reason](/admin/overview#dangerous-actions-ask-for-a-reason).
