# Administration: identity providers, SCIM and client applications moved to their own pages (#2537, ADR-0059 R3.6)

**What changed.** The admin console has four new pages: **People and access ›
Access** (`/admin/app/access`), **Identity providers**
(`/admin/app/identity-providers`) and **SCIM** (`/admin/app/scim`), and
**Platform › Client applications** (`/admin/app/client-applications`, with the
native clients and the Microsoft apps as two tabs). The identity provider,
SCIM, native client and Microsoft app editors are no longer rendered on the
**Configuration** page. Its **Authentication**, **Native clients** and
**Microsoft apps** sections stay in the list, as links to the new pages, until
the server stops declaring those managed surfaces (R4.9).

The temporary **Roles** link under People and access (see
`2026-11-24-2534-admin-view-only-mode.md` and
`2026-11-25-3042-auditor-roles-link-removed.md`) is gone. The **Access** item
replaces it, and is shown to every role that opens it, the security auditor
included (at view). The permission matrix keeps its address,
`/admin/app/roles`, is reached from Access by a caller who can read it, and
marks Access as the active item.

On Identity providers and SCIM, the trust fields and actions are disabled,
with a notice, for a caller who does not hold `identity.sso.trust.manage` or
`admin.auth.users.super_admin` (a new `identity-trust` item of the console
capability document). That includes the platform administrator. The server
already refused those changes with `identity_provider_trust_super_admin_required`
and `scim_client_super_admin_required`; it still does.

With the six labelled areas off, the two-group navigation lists the four new
pages too.

**Who is affected.** Administrators who open the identity or client editors
from Configuration, and any automation or bookmark that clicks those sections.
The security auditor now sees **Access**. No permission is granted or removed,
and no existing API changes.

**What to do.** Update bookmarks and runbooks to the new addresses. End-to-end
tests that clicked **Configuration › Authentication** or **Native clients**
should open the new pages. No operator action is needed on the server.
