# Admin console: dangerous actions send a reason (#2614, ADR-0059 R3.8b)

**What changed.** The admin console confirms every dangerous action the server's
action table marks in one dialog, and sends the reason with the request: the
`reason_code` and `reason` body fields, or the percent-encoded
`Elitea-Reason-Code` and `Elitea-Reason` headers for a request without a JSON
object body. The old per-page confirmations (branding reset, project delete,
role delete, SCIM client and binding, platform model and provider delete,
credential revoke, legal hold release) are gone; the platform-wide kill switch
asks for `STOP` in the same dialog instead of in its form. Every save of the
Configuration › Maintenance section asks too: turning maintenance mode on is
the marked action, and the server cannot tell it from another save of the
section by the request alone. The `break_glass` section has no console editor
yet, so nothing asks for it.

**Who is affected.** Administrators, and anything that drives the console's
dialogs by their old test ids or labels (end-to-end suites, browser automation).
Scripts that call the marked routes directly are not affected by this change,
but the server warns with `Elitea-Reason-Missing: 1` and will refuse a missing
reason after its warning period.

**What to do.** No operator action for the console. Update browser automation
to the `admin-dangerous-*` test ids, and start sending `reason_code` (and a
`reason` for `other`) from scripts that call dangerous admin routes.
