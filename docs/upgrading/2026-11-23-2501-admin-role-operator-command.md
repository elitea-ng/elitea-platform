# Administration: the `elitea-main admin-role` operator command (#2501, ADR-0059)

**What changed.** The elitea-main image has a new operator command,
`elitea-main admin-role`, with four sub-commands:

- `list`: every administration role, its built-in marker and its holders;
- `grant USER ROLE` and `revoke USER ROLE`: give or take one administration
  role (USER is a numeric id or an e-mail address);
- `reset ROLE`: put a built-in role with a declared set back to that set.

It runs inside the main workload (`kubectl -n <ns> exec deploy/elitea-main --
/elitea-main admin-role ...`), reads `DATABASE_URL` and is not reachable over
HTTP or gRPC. Each change takes the role-writes lock, writes a grant history
record with the source `operator` and a security audit event with the source
`operator`; `reset` and every use of `--override-last-administrator` are
`critical`. Exit codes: 0 done, 1 failed, 2 invalid invocation, 3 refused by a
rule.

**When to use it.**

- A deployment has no administrator left (the last one left, lost their
  sign-in, or was deprovisioned): `admin-role grant <e-mail> super_admin`.
- The first administrator, when `identity.initial_global_admins` is not set
  or has been used.
- Removing the last administrator on purpose: `admin-role revoke ...
  --override-last-administrator`. The console and the API refuse it (#2500).
- A built-in role whose locked set was edited before the upgrade or by hand:
  `admin-role reset <role>`. It refuses `editor`, `viewer`, `system` and
  custom roles, and refuses to run from an image older than the database's
  newest shared migration.

**What to do.** Nothing on upgrade. Grant access to `kubectl exec` in the
elitea-main Deployment only to the people who may make these changes: that
access is the control. The full reference is "Administrative roles from the
command line" in the deployment operations guide.
