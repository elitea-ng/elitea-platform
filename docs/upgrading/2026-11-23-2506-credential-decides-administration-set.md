# The credential decides the administration rights (#2506, ADR-0059)

**What changed.** Administration permissions are no longer the same for every
credential a person holds. The platform now resolves them from the credential
that sent the request:

| Credential | Administration permissions |
|---|---|
| Browser session (OIDC, SAML or Form sign-in) | All of the owner's. |
| Personal access token, `admin_access` `none` | None. |
| Personal access token, `admin_access` `inherit` | All of the owner's, until the sunset date a later release announces. |
| Personal access token, `admin_access` `scoped` | The token's scopes that the owner still holds. Scopes cannot be chosen yet. |
| Desktop or mobile device session | Only the maintenance bypass and Marketplace moderation. |
| Agent run token | None. |
| Project system token | None. |

A request with `X-API-Key` follows the credential the header carries: a
personal access token or a run token. Only the administration permissions
change. Project permissions and developer permissions are resolved as before,
for every credential.

**New tokens.** Shared migration `0265_token_admin_access.sql` (#2505) gave
every new token `none`. From this release a token with `none` has no
administration rights. This includes a token created after the 0265 upgrade by
a person who already held an administration role. Tokens that existed at the
0265 upgrade and belonged to an administrator are `inherit` and keep working.

**Desktop and mobile.** A device session of an administrator keeps the two
checks the apps make in place: it can work during a maintenance window and can
moderate the Marketplace. No administration API answers it any more, including
the routes that accept `runtime.plugins`. The administration console was never
part of the apps, so the apps need no update.

**Run and system tokens.** An agent run of an administrator, and the
scheduled index run with the project system token, have no administration
rights. Both keep their project permissions, so neither changes what it does.

**Form sessions.** Shared migration `0266_form_session_ref.sql` adds a
reference column, `ref`, to `elitea_auth.form_sessions`. It draws from the
same sequence as `elitea_auth.browser_sessions.ref`, so a Form session and a
single sign-on session never share a number. Existing sessions get a value
during the upgrade, so no one is signed out. The column is additive, and a
replica of the previous release writes rows that get the default. Runs started
from a Form session now record it in `execution_jobs.browser_session_ref`, as
runs from a single sign-on session already did.

**Who is affected.** Automation that calls an administration API with a
personal access token created after the 0265 upgrade now gets `403`.
Automation with an `inherit` token is not affected. Agents that called
administration APIs with their run token also get `403`, and so do desktop and
mobile clients that called them directly.

**What to do.**

- Find the tokens that hold no administration rights:
  `SELECT id, user_id, name FROM public.auth_core__token WHERE uuid IS NOT NULL AND admin_access = 'none';`.
- Use an administrator's browser session for administration work. Choosing
  administration scopes for a token comes in a later release. Until then, an
  operator who must keep a new token working can set it to `inherit` in the
  database:
  `UPDATE public.auth_core__token SET admin_access = 'inherit' WHERE id = <token id>;`.
  The token then holds all of its owner's administration permissions until the
  sunset date.
- A rolling upgrade is safe. A replica of the previous release still resolves
  every credential to its owner's full set until it is replaced, so finish the
  rollout before you rely on the narrower answer.
