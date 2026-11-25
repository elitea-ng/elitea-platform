---
date: 2026-11-25
---
## Dangerous administration actions take a reason

Deleting a user, changing who is an administrator, releasing a legal hold and the other dangerous administration actions now accept a reason: a code such as `incident` or `offboarding` and an optional text. The audit trail records it, so a later review can see why a change was made. Automation that sends no reason keeps working for now; its responses carry `Elitea-Reason-Missing: 1` as a warning, and after the sunset date such calls will be refused. Keep the `Elitea-Reason` header out of your proxy access logs. See [Reasons for dangerous actions](/admin/audit-trail#reasons-for-dangerous-actions).
