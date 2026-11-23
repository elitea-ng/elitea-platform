# Administration: e-mail, operations and the MCP catalogue accept narrow permissions (#2493, ADR-0059)

**What changed.** Eleven administration routes now accept a narrow permission
beside `runtime.plugins`. Before this release they accepted only
`runtime.plugins`:

| Permission | Routes |
|---|---|
| `platform.email.manage` | Read and save the e-mail settings; send a test message (`/admin/email/administration`, `/admin/email/test/administration`). |
| `platform.operations.view` | System information (`/admin/system_info/{mode}`), the tracing status (`/tracing/status/administration`) and the Tasks list (`GET /admin/background_jobs/administration`). |
| `platform.operations.manage` | Cancel a background job and run the token expiry notices (`POST /admin/background_jobs/administration/...`). |
| `mcp.catalogue.manage` | List, save and delete prebuilt MCP servers (`/admin/mcp_prebuilt_servers/administration`). |

Shared migration `0261` (#2491) already granted the four names to `super_admin`
and `admin`, so this release adds no migration.

**Who is affected.** Nobody loses access. A holder of `runtime.plugins` sees no
change: every route answers that holder as before. A role that holds only one of
the narrow names can now reach the routes of that name and nothing else.
`platform.operations.manage` does not include reading the Tasks list. Grant it
together with `platform.operations.view`.

The navigation still names `runtime.plugins` for these pages, so a role with
only a narrow name reaches the routes but does not see the pages in the menu yet.
A later release changes the navigation.

**What to do.** Nothing.
