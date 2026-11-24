# Administration: Configuration sections accept a narrow permission (#2494, ADR-0059)

**What changed.** Each Configuration section now declares its console area and
a read and a write permission. A request for a section passes on one of two
paths:

- **Narrow path (new).** The section's read permission alone opens it (`GET`);
  its write permission alone opens and saves it (`GET` and `PUT`).
- **Legacy path (unchanged).** `runtime.plugins` together with the section's
  `required_permission`, for both verbs, as before.

The three Configuration routes (`GET /api/v2/admin/plugin_config_schemas/{mode}`,
`GET` and `PUT /api/v2/admin/plugin_config_values/administration/{section}`,
`GET /api/v2/admin/plugin_config_suggestions/{mode}/{key}`) accept
`runtime.plugins` or any narrow permission a section declares. The handler then
decides per section. The list of names is computed from the sections.

The pairs follow ADR-0059 decision 2: the guardrail section reads with
`llm.policy.view` and saves with `llm.guardrails.manage`; the breaker and LLM
governance sections use `llm.policy.view` and `llm.policy.manage`; e-mail uses
`platform.email.manage`; maintenance uses `platform.maintenance.manage`; the
identity sections (Authentication, Session Policy) use `identity.sso.view` and
`identity.sso.settings.manage`; MCP Servers uses `mcp.catalogue.manage`;
branding, native clients and Microsoft apps keep their own single permission for
both halves; Service Descriptors, a section with no fields that points at its
own page, reads with that page's `runtime.airun.serviceproviders` and saves
with `configuration.service_descriptors` (no migration grants that name, so
otherwise no administrator would see the pointer); every other section uses
`platform.config.view` and `platform.config.manage`. The agent governance
sections and Entitlement Export keep the legacy path only. The full table is in
the administrator guide, "Features and configuration", "Who can open a
section".

The schema list and the suggestions are filtered to the sections the caller can
read. A section the caller cannot open is no longer in the list, and a
suggestion source that only such sections declare answers like an unknown
source (400). Each listed section carries its area and pair as `access`, beside
`required_permission`, which stays for the web until the console groups the
page by area.

Shared migration `0261` (#2491) already granted the narrow names to
`super_admin` and `admin`, so this release adds no migration.

**Who is affected.** Nobody loses access to a section. A holder of only
`configuration.governance` is still refused on the three routes. A holder of
`runtime.plugins` and `configuration.governance` gets the same answer on every
section as before. Such a holder no longer sees, in the schema list, the
sections whose values already answered 403 to them (for example Branding without
`configuration.branding`). A holder of `runtime.plugins` alone no longer gets
the `projects` suggestions, which only the LLM Governance section declares and
which that holder cannot open.

Some holders gain access. Before, every section also needed `runtime.plugins`.
Now a role that holds a section's narrow permission without `runtime.plugins`
reads that section, and saves it when the name is its write permission. This
also covers the sections that keep their single permission: a custom role with
`configuration.branding`, `configuration.native_clients`,
`configuration.microsoft_apps` or `configuration.service_descriptors` and no
`runtime.plugins` can now read and save the Branding, native client (Native
clients, Native client policy, Cloud executor), Microsoft apps or Service
Descriptors section through the Configuration routes. The first three names
already opened their own pages (Branding, native clients, Microsoft apps)
without `runtime.plugins`. Review custom
roles that hold one of these names if they should not edit the section.

**What to do.** Nothing. A custom role that should manage one section can now be
given that section's narrow permission instead of `runtime.plugins`.
