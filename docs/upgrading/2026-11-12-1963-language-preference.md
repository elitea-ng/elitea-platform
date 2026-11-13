# Interface language is stored per user (issue 1963)

- **What changed.**
  - **New `GET` / `PUT /social/author/language`** (client contract 1.51). Stores the caller's interface language, a
    language tag such as `de` or `pt-BR`, in `centry.social_users.personalization` under `language`, beside
    `theme_mode`. A malformed tag is a 400. It is also returned as `personalization.language` by `GET /social/author`.
  - **`PUT /social/author` no longer writes `personalization.language`.** Like `theme_mode`, the stored value is
    carried forward and a value in the request body is dropped.
  - **Web and desktop** save the Language choice from the account menu to the server and apply the server value after
    sign-in; the browser-local choice stays as the boot cache.
- **Who is affected.** A client that wrote `language` into `personalization` through `PUT /social/author` (the bundled
  clients do not): use the new route. Nothing to do for operators; no migration (the key lives in the existing jsonb
  blob).
