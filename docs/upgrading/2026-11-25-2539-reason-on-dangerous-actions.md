# Administration: dangerous actions take a reason (#2539, ADR-0059 R3.8a)

**What changed.** Every dangerous or critical administration action now
accepts a reason, and the admin audit row records it. This is the warning
period of ADR-0059 decision 6: a missing reason is not refused yet.

- Send the reason in the JSON body fields `reason_code` and `reason`, or in
  the request headers `Elitea-Reason-Code` and `Elitea-Reason` when the
  request has no body (most `DELETE` requests). A body field wins over its
  header. The server takes both fields out of the body before the handler
  reads it, except `reason` on a route whose own request already has one (a
  credential revoke, a suspension, a legal hold release). The permission
  matrix save (`PUT /api/v2/admin/user_project_permissions/administration`)
  reads the headers only, because its body keys are role names.
- `reason_code` is one of `incident`, `request`, `maintenance`,
  `correction`, `offboarding` or `other`. `other` needs a `reason` text.
- The `Elitea-Reason` header is percent-encoded UTF-8 (`encodeURIComponent`
  in a browser) of at most 500 bytes after decoding. A body `reason` is at
  most 2000 bytes.
- A request without a reason code is served as before, and its response
  carries `Elitea-Reason-Missing: 1`. Its audit row has
  `reason_missing = true`. Personal access tokens are treated the same way.
- A reason the server cannot accept answers `400` with the code
  `reason_invalid`: an unknown code, `other` without a text, a header that
  does not decode, is sent twice or is too long, or a body field that is not a
  string. This applies now, during the warning period.
- The reason text is stored on the admin audit row only. It is not part of
  the tamper-evident chain and is never exported. The reason code is also
  carried by the security events (`admin.access.changed`).

**No migration.** The audit columns `reason_code`, `reason` and
`reason_missing` were added by shared migration 0268 (R1.4a).

**Who is affected.** Automation that calls a route in the table below. It
keeps working. Watch for `Elitea-Reason-Missing: 1` in its responses: after
the sunset date of ADR-0059 decision 2, the same request will answer `400`
with the code `reason_required` (story R4.8, issue 2554).

**What to do.**

1. Add `reason_code` (and `reason` where it helps a reviewer) to every call of
   a route below.
2. Make sure your reverse proxy, ingress and load balancer access logs do not
   record the `Elitea-Reason` request header. The text can hold personal data.
   See the deployment hardening checklist.

**The marked actions.** A route that carries several actions is marked per
action; a `{mode}` route is marked in the `administration` mode only. A
retention write that may shorten the policy is marked as a whole: send a
reason on every write of it.

| Route | Action | Danger |
|---|---|---|
| `POST /api/v2/admin/auth_users/{mode}` | `delete` | dangerous |
| `POST /api/v2/admin/auth_users/{mode}` | `set_admin_role` | critical |
| `DELETE /api/v2/admin/branding/administration` | whole route | dangerous |
| `POST /api/v2/admin/credential_inventory/revoke` | whole route | dangerous |
| `POST /api/v2/admin/execution_suspensions` | `platform` | dangerous |
| `POST /api/v2/admin/gateway/governance` | `guardrail` | critical |
| `DELETE /api/v2/admin/gateway/governance/{id}` | `guardrail` | critical |
| `PUT /api/v2/admin/gateway/governance/{id}` | `guardrail` | critical |
| `DELETE /api/v2/admin/gateway/platform_models/{configID}` | whole route | dangerous |
| `POST /api/v2/admin/gateway/providers` | whole route | critical |
| `DELETE /api/v2/admin/gateway/providers/{configID}` | whole route | dangerous |
| `PUT /api/v2/admin/gateway/providers/{configID}` | `route` | critical |
| `DELETE /api/v2/admin/identity_providers/administration/{key}` | whole route | critical |
| `PUT /api/v2/admin/identity_providers/administration/{key}` | `disable` | critical |
| `PUT /api/v2/admin/identity_providers/administration/{key}` | `trust` | critical |
| `POST /api/v2/admin/identity_providers/administration/{key}/rebind` | whole route | critical |
| `POST /api/v2/admin/invites_bulk/administration` | whole route | dangerous |
| `POST /api/v2/admin/legal_holds/{id}/release` | whole route | critical |
| `DELETE /api/v2/admin/modes/administration` | whole route | critical |
| `POST /api/v2/admin/modes/administration` | whole route | critical |
| `POST /api/v2/admin/permissions/{scope}/{mode}` | whole route | critical |
| `PUT /api/v2/admin/permissions/{scope}/{mode}` | whole route | critical |
| `PUT /api/v2/admin/plugin_config_values/administration/{plugin}` | `break_glass` | critical |
| `PUT /api/v2/admin/plugin_config_values/administration/{plugin}` | `maintenance:on` | dangerous |
| `PUT /api/v2/admin/retention/policies/{dataClass}` | `shorten` | critical |
| `DELETE /api/v2/admin/retention/policies/{dataClass}/projects/{projectID}` | `shorten` | critical |
| `PUT /api/v2/admin/retention/policies/{dataClass}/projects/{projectID}` | `shorten` | critical |
| `DELETE /api/v2/admin/roles/{scope}/{mode}` | whole route | critical |
| `POST /api/v2/admin/scim_clients/administration` | whole route | critical |
| `DELETE /api/v2/admin/scim_clients/administration/{id}` | whole route | dangerous |
| `POST /api/v2/admin/scim_clients/administration/{id}/revoke` | whole route | dangerous |
| `POST /api/v2/admin/scim_clients/administration/{id}/rotate` | whole route | critical |
| `POST /api/v2/admin/scim_group_bindings/administration` | whole route | critical |
| `DELETE /api/v2/admin/scim_group_bindings/administration/{id}` | whole route | critical |
| `PUT /api/v2/admin/scim_group_bindings/administration/{id}` | whole route | critical |
| `PUT /api/v2/admin/user_project_permissions/administration` | whole route | critical |
| `PUT /api/v2/admin/user_suspend/{mode}/{userID}` | `suspend` | dangerous |
| `DELETE /api/v2/projects/project/{mode}/{projectID}` | whole route | critical |
