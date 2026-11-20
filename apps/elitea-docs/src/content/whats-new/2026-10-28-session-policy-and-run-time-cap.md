---
date: 2026-10-28
order: 4
---
## Session policy and run time cap

Platform administrators can set a maximum idle timeout and a maximum lifetime for browser sessions, and a maximum time for one agent run. Every session start, expiry, sign-out and revocation is now an audit event. Stopping a toolkit type with the kill switch now also cancels the runs in progress that use it. All new limits are off until you set them. See [Session policy](/admin/authentication#session-policy) and [Runtime safety](/admin/enterprise-controls/runtime-safety#run-caps).
