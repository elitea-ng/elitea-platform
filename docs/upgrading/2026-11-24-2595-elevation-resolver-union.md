# Administration: the permission resolver reads the break-glass ledger (#2595, ADR-0059)

**What changed.** For a browser session that has a server-side row (an
`elitea_session` single sign-on session or a Form session), the
administration permissions of a request now also include the permissions of
the level's role (`admin` or `super_admin`) of every elevation in
`elitea_auth.admin_elevations` (shared 0269) that belongs to the same person,
is bound to the same session, has started and not ended, and has not expired.
The level is joined to the role by name at each request, so a later grant
migration reaches an active elevation, and an elevation stops granting at its
expiry with no job running. A personal access token, a desktop or mobile
session, a run token and a session without a server-side row never match.

The role rules (not yourself, your own role is locked, only a super admin
assigns `super_admin`, the subset rule), the global administrator's exemption
and the project self-membership check now read the caller's standing roles
through a separate standing-set call, which never includes an elevation. The
project access check still tests the standing role named `super_admin`, so an
elevation does not open project content.

**Who is affected.** Every deployment. Nothing changes until an elevation
exists, and in this release nothing can start one yet: the request routes and
the sealed sign-in come in later releases. The resolver issues one more
indexed query (on `admin_elevations_open_session_idx`) for an
administration-mode check made from a browser session with a server-side row.

**What to do.** Nothing.
