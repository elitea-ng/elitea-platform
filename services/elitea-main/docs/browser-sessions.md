# The server-side browser session

Status: implemented. Shared migration `0117_browser_sessions.sql`.
Code: `internal/auth/browsersession`, `internal/api/middleware/auth.go`,
`internal/api/v2/auth/{session,signin,oidc,saml}.go`.

This note records a decision that is smaller than an ADR and larger than a
comment. The architecture record for identity is ADR-0021 (single sign-on) and
ADR-0017 (the removal of `AUTH_DEV_MODE`); this note states what a session *is*
in this service, and what a deployer must know.

## What the session was, and why it changed

Before this change, a browser that signed in through OIDC or SAML got a
SELF-CONTAINED cookie. `makeSessionToken` put `uid`, `email` and a 24-hour
`exp` into a base64 payload and appended an HMAC of that payload, keyed with
`APPLICATION_SECRET_KEY`. Nothing on the server recorded that a session
existed.

Three properties were therefore impossible.

1. **Revocation.** Logout deleted the browser's copy of the cookie. Any other
   holder of the same value kept using it until `exp`.
2. **An idle deadline.** The token carried one absolute deadline. A tab left
   open all day held the same credential as one used continuously.
3. **A provider record.** SAML single logout names the assertion's
   `SessionIndex`, and OIDC end-session names the session it is ending. A
   signed cookie carries neither, so `/forward-auth/auth_saml/logout` is
   mounted as a LOCAL clear only.

The smoke run after 1.60.0 found the user-visible half of the same gap: an
expired user on an open page got no redirect to the identity provider.

## What the session is now

One row of `elitea_auth.browser_sessions`. The row holds the user id, the
display address, the provider that created it, the SAML session index, the
three timestamps, the idle timeout it was issued under, and `revoked_at`.

**The cookie carries the identifier and nothing else.** The name is unchanged —
`elitea_session` — and the value is `s1.` followed by 32 random bytes in
unpadded base64url. `HttpOnly`, `Secure` (unless `COOKIE_SECURE=false`),
`SameSite=Lax`, path `/`. `MaxAge` is the absolute lifetime.

The name was kept because four handlers write it, one middleware reads it, and
the SPA's boot probe depends on it. A new name would have to change all of them
at once, and a deployment mid-rollout would hold both. The VALUE format changed
instead, and the two formats cannot be confused: a legacy value's first segment
is base64url of a JSON object, which always begins `ey`.
`browsersession.LooksServerSide` is the single discriminator.

### Lifetimes

| Knob | Default | Meaning |
| --- | --- | --- |
| `ELITEA_SESSION_IDLE_TIMEOUT` | `8h` | Measured from `last_seen_at`. `0` turns it off. |
| `ELITEA_SESSION_ABSOLUTE_LIFETIME` | `168h` | Measured from creation. Never moves. |
| `ELITEA_SESSION_REJECT_LEGACY_COOKIES` | off | Refuse the pre-0117 signed cookie. |

Both lifetimes are stamped ONTO the row at creation. A deployment that shortens
its idle window does not retroactively shorten sessions issued under the old
one, and one that lengthens it does not silently extend them.

A mistyped duration stops the boot. A deployment that meant to set a lifetime
must not run for a week believing it did.

### Revocation and the touch throttle

`GET /forward-auth/logout` (and its `/auth_form/logout` and `/auth_oidc/logout`
aliases) revokes the row before it clears the cookie. Revocation keeps the
first revocation's timestamp, so a browser retry does not rewrite when the
session ended. `Manager.RevokeUser` ends every session of one account.

`last_seen_at` moves at most once a minute. Without the throttle every
authenticated request would be an `UPDATE` on one row per user. The throttle is
also repeated in the `WHERE` clause, so two replicas cannot write an older
timestamp over a newer one. A failed touch never refuses a caller: the row was
READ, so the answer about the caller is already known.

### Why Postgres and not Redis

This service already keeps one kind of auth state in Redis —
`internal/infra/authsession`, the Form graph's login-transaction store. Losing
that store costs a user one retry of a login they are in the middle of. Losing a
browser-session store signs everybody out. An OIDC-only deployment — the shape
the Kubernetes chart and the standalone stack both run — composes no Redis at
all. Postgres is present in every deployment.

## The SPA contract

`GET /forward-auth/info` has three answers, and they are not
interchangeable.

| Condition | Status | Body |
| --- | --- | --- |
| No cookie at all | `200` | `{"authenticated": false}` |
| A cookie that no longer works | `401` | `{"authenticated": false, "error": {"code": "session_expired", …}, "login_url": "…"}` |
| The store did not answer | `503` | `{"error": {"code": "session_store_unavailable", …}}` plus `Retry-After` |

The 401 also carries the login URL in a `Location` header. It is a HINT: no
browser follows `Location` on a 401, and this response is read by an
`XMLHttpRequest` in any case. Both spellings carry the same URL so a caller can
read whichever it already reads. `?target_to=` on the probe is preserved through
`browserflow.CanonicalReturnTarget`, so the browser returns to the page it was
on and nothing else.

**Only the primary check redirects.** The app shell navigates on
`session_expired` from its own session probe. A 401 from the notification bell,
a permission read or any other peripheral call must NOT move the browser: a
peripheral 401 once re-authenticated in a loop forever, and the transport still
carries `background: true` for exactly that reason.

The middleware's refusals follow the same split. A revoked, expired, idle or
unknown session is `401`. A store that could not be READ is `503` with
`Retry-After`, because a `401` there signs out every browser for as long as the
database is unreachable.

## Who reads the cookie

Three readers, and all three must know both formats, or a deployment gets a
half-signed-in browser:

* `apimw.Auth` — every `/api/v2` request.
* `SessionHandler.Info` — the app shell's probe.
* `adminui.Handler.ServeSPA` — the admin console's page shell, which injects
  the operator's id, address and permission list into the served HTML. It had
  already produced an EMPTY ADMIN SIDEBAR once, by reading one credential
  source and not the other; a cookie it cannot read reproduces that exactly.

## The legacy-cookie window

A deployment that upgrades holds unexpired signed cookies issued by the previous
release. They keep working:

* `apimw.Auth` reads a value without the `s1.` prefix with the old HMAC reader.
* `/forward-auth/info` does the same.
* Every NEW sign-in mints a server session.

The longest a legacy cookie survives on its own is 24 hours, which is its `exp`.
An operator who would rather force one re-login than keep an unrevocable
credential alive sets `ELITEA_SESSION_REJECT_LEGACY_COOKIES=true`; every legacy
cookie is then refused with `legacy_session_cookie_rejected` in the log, and the
browser re-authenticates.

Removing the legacy reader entirely is a follow-up for the release after the
one that ships this.

## What is still open

* **SAML single logout.** The row now carries the `SessionIndex` a
  `LogoutRequest` must name, which is what made single logout unbuildable
  before. `/forward-auth/auth_saml/logout` still clears the local session only,
  and `internal/api/router.go` says so.
* **OIDC end-session.** Same shape: the row records the provider, and nothing
  yet calls the provider's `end_session_endpoint`.
* **A sweeper.** `PostgresStore.DeleteExpired` exists and nothing schedules it.
  The table grows until something does; the rows are small and the index on
  `expires_at` is there for it.
* **Suspension does not revoke.** `authsvc.NewPrincipalValidator` already
  refuses a suspended account on every request, so a suspended user cannot act.
  Their session row stays until it expires.
