# Administration access on access tokens (#2505, ADR-0059)

**What changed.** Shared migration `0265_token_admin_access.sql` adds two
columns to `public.auth_core__token`:

- `admin_access` (`text NOT NULL DEFAULT 'none'`), one of `none`, `scoped` or
  `inherit`.
- `admin_scopes` (`text[] NOT NULL DEFAULT '{}'`), the administration
  permissions of a `scoped` token. It stays empty for `none` and `inherit`.

A token created after the upgrade is recorded as `none`. Once a later ADR-0059
release enforces the value, such a token has no administration rights until
scopes are chosen. This holds for personal access tokens, desktop and mobile
device sessions, and project system tokens.

The migration sets existing rows once:

- A personal access token whose owner holds at least one administration
  permission at upgrade time gets `inherit`. Such a token keeps its owner's
  administration permissions until the sunset date that a later release
  announces. A role without permissions does not count.
- Every other personal access token gets `none`. Its owner holds no
  administration permission today, so the token resolves no administration
  permission today either. It does not gain any when its owner gets an
  administration role later.
- Desktop and mobile device sessions and project system tokens get `none`.

**Nothing is enforced yet.** In this release nothing reads the two columns, so
no request is answered differently: every token, new or old, still resolves its
owner's administration permissions. Do not treat a new token as having no
administration rights before the enforcing release. A later ADR-0059 release makes the
resolver read them; its upgrade note says what changes then.

**Who is affected.** No one in this release. If you back up or replicate
`public.auth_core__token`, the table has two more columns. Later, automation
that uses a personal access token created after this upgrade for an
administration API needs administration scopes on that token.

**What to do.**

- Nothing to do now.
- A rolling upgrade is safe. A replica of the previous release writes new
  tokens without the columns, and they get `none`, the same as from this
  release.
- To see which tokens were set to `inherit`, run
  `SELECT id, user_id, name FROM public.auth_core__token WHERE admin_access = 'inherit';`.
