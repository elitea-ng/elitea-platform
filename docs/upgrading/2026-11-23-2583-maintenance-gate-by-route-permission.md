# Maintenance windows admit an administrator to the routes of their permission (#2583, ADR-0059 R0.4b)

**What changed.** During a maintenance window the API used to admit only a
holder of `runtime.plugins`. It now reads the permission registry:

- A holder of `platform.maintenance.manage` or `runtime.plugins` uses the
  whole application, as before. The platform settings read answers
  `bypass: true` for them, so the app shows them no splash. A desktop or
  mobile session keeps both names.
- A holder of any other administration permission reaches the administration
  routes that permission opens, wherever they are mounted (for example the
  project budgets and the audit trail under `/api/v2/elitea_core`, project
  create and delete, the administration secrets), with the same answers as
  outside a window. Every other route answers the maintenance `503`. Before,
  such a caller got the `503` everywhere outside `/api/v2/admin`.
- A caller with no administration permission gets the `503`, as before.

`GET` and `PUT /api/v2/admin/maintenance/administration` accept
`platform.maintenance.manage` beside `runtime.plugins`.

**Who is affected.** Nobody today: the built-in administrator roles hold
`runtime.plugins`, and their answers are unchanged with the window on and off.
The change matters once a role holds only narrow administration permissions
(the built-in roles of ADR-0059 R0.7, or a custom role).

**Operator action.** None.

The administration surface test now also reaches the maintenance bypass, the
token mint of a desktop or mobile session and the Marketplace moderation
check (#2925). That is a test change with no effect on a deployment.
