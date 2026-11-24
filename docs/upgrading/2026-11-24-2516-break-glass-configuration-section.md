# New Configuration section: Break-glass (#2516, ADR-0059 R2.2a)

**What changed.** The admin **Configuration** page has a new section,
**Break-glass** (`break_glass`): the durations, caps, request lifetime,
review times, re-authentication window and approval switch of break-glass
elevations. No route starts an elevation in this release; the section stores
the policy the request, start and sealed paths will apply when they ship.

**Who can change it.** Only a holder of `access.breakglass.manage` sees or
saves the section (seeded to `super_admin` and `admin` by shared migration 0261; it is
never granted through the API). A holder of `runtime.plugins` alone gets
`403`. Saving it is a `critical` action: refused under an elevation and to
every personal access token.

**The rules.**

- Defaults: approval on; 60/240 minutes online (default/cap); 60/120 minutes
  sealed; a 30-minute request lifetime; review within 72 hours (24 after a
  sealed sign-in); a 15-minute re-authentication window; a 24-hour
  approval-off delay.
- A deployment can lower any value and raise a cap only up to the ceilings in
  code, 480 minutes online and 240 sealed. A save above them, or a default
  above its cap, answers `400`.
- Switching approval off needs `admin.auth.users.super_admin` from a standing
  role (`403` otherwise), takes effect after the delay in force before the
  save, and writes the security audit event
  `admin.access.break_glass_approval` (reason `approval_off_requested`) at
  once. Switching it back on takes effect at once, cancels a pending
  switch-off and writes the event with `approval_on_restored`.
- Shortening the approval-off delay needs the same standing super_admin
  (`403` otherwise); lengthening it does not.

**The new audit kind.** `admin.access.changed` events gain the kind
`break_glass_approval` (`libs/jsonschema/audit/v1/event.schema.json`). A SIEM
rule that enumerates the kinds should add it.

**What to do.** Nothing is required. Review the defaults on the Break-glass
section if your policy differs.
