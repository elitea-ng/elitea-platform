# Sealed break-glass sessions: a fourth browser session kind and its cookie (#2521, ADR-0059 R2.7a)

**What changed.**

- Shared migration `0272_sealed_browser_sessions.sql` replaces the CHECK
  `browser_sessions_provider_known` of `elitea_auth.browser_sessions` with
  the same constraint plus the value `sealed`. It is a widening: every
  existing row stays valid, and no row is written. It does nothing on a
  database without the table.
- A sealed session is a `browser_sessions` row of provider `sealed`, created
  only together with a sealed elevation, in one transaction. It is carried
  by its own cookie, `__Host-elitea_sealed` (`Secure`, `HttpOnly`,
  `SameSite=Strict`, `Path=/`, no `Domain`), is honoured on the
  administration surface only and ignored everywhere else, holds its
  elevation's level and nothing of the person's standing roles, never calls
  the identity provider, and ends with its elevation: ending an elevation on
  the sealed path (by its holder, an approver, `break-glass end` or its
  expiry) also revokes the session row.
- The ordinary `elitea_session` cookie never authenticates a sealed row. A
  sealed cookie that is no longer live is cleared and counts as absent, so
  an ordinary session in the same browser still decides the request.
- The session policy's idle maximum, when set, also applies to a sealed
  session.
- No route creates a sealed session yet: the sign-in page that does ships in
  a later release.

**Who is affected.** Deployments that want the sealed break-glass path must
serve elitea-main over HTTPS: browsers refuse a `__Host-` cookie from a
plain-HTTP origin, so on such a deployment the sealed path is unavailable.
Nothing else changes for existing sessions. A proxy or WAF that filters
cookies by name must pass `__Host-elitea_sealed` through to elitea-main.

**What to do.** Nothing on upgrade. See
[Break-glass access](../../apps/elitea-docs/src/content/docs/admin/break-glass.mdx)
for what a sealed session can reach.

**Rollback.** The wider CHECK stays (it admits every row the old one did).
The previous release does not know the `sealed` provider and would not
refuse such a row behind the ordinary cookie. No sealed session exists
before the sign-in page ships; once it has, run `break-glass end --all`
before rolling back, which revokes every sealed session.
