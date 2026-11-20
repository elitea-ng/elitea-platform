---
date: 2026-10-30
order: 0
---
## Entitlement export for access reviews

Administrators can download who holds which role on the platform, on each project and on each agent, with what each role allows (configure agents, approve change requests, attest, approve connectors and more), as CSV from the admin console's **Users** page. Identity governance tools can read the same data over a read-only SCIM tree at `/api/v2/scim/v2/entitlements`; no SCIM client can read it until an administrator lists it under **Configuration › Entitlement Export**. Every export is audited. See [Least privilege and trust](/admin/enterprise-controls/least-privilege-and-trust#entitlement-export-for-access-reviews).
