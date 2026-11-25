---
date: 2026-11-25
---
## Identity providers, SCIM and client applications have pages of their own

The admin console's **People and access** area now has **Access**, **Identity providers** and **SCIM**, and **Platform** has **Client applications**, with the native desktop and mobile clients and the Microsoft app registrations as two tabs. The identity and client editors used to be sections of **Configuration**; those sections now link to the new pages. **Access** lists every administration role, what it opens in the console, and whether you can give it to someone, and links to the permission matrix for those who can read it. The security auditor can now read Access. On **Identity providers** and **SCIM**, the trust controls (adding or deleting a provider, changing its issuer, certificates or secret, and creating, rotating or deleting a SCIM client) are disabled with a notice for anyone who is not a global administrator, so you see what the server would refuse before you try. See [Authentication](/admin/authentication) and [Users and roles](/admin/users-and-roles#role-definitions).
