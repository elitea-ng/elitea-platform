# Admin Guardrails tool-map rows

Status: `complete` for the Web defect. No Main, Worker or contract change.

## Defect and business behaviour

On `/admin/app/configuration` → Guardrails, a global admin clicking
**Add toolkit — Sensitive Action Tools** or **Add toolkit — Blocked Tools** got
no row, so neither policy could be extended from the page. The backend was
correct: `PUT /api/v2/admin/plugin_config_values/administration/guardrails`
stored and returned `sensitive_tools = {mcp: ["echo"]}`.

Behaviour kept from the current platform (Pylon `admin_ui` Guardrails form): an
operator adds a toolkit row, names the toolkit, lists its tools, and saves the
`{toolkit type: [tool names]}` map. A blank, unsaved row is not stored.

Not ported: nothing new. The current platform's raw JSON fallback for object
fields stays replaced by the structured editor.

## Cause

`ToolMapField` derived the editor rows from the stored map on every render
(`toConfigToolMapRows(value)`) and wrote rows back through
`fromConfigToolMapRows`, which drops a blank toolkit by design. A new blank row
therefore had no key, and vanished on the same render. The same derivation also:

- re-sorted a half-typed toolkit above its neighbours, under the cursor;
- merged two rows that canonicalise alike before the duplicate warning could
  show.

The existing editor tests used a harness that held rows in its own state, so
they never exercised the page's value round trip.

## Change

| Path | Change |
| --- | --- |
| `apps/elitea-web/src/pages/admin/ConfigurationToolMapEditor.tsx` | New `useConfigToolMapRows(value, onValueChange)`. Rows are editor state; they are re-derived only when `value` is not the map the editor itself last emitted (compared by reference). First load, Discard and the post-save refetch replace the rows. |
| `apps/elitea-web/src/pages/admin/ConfigurationSectionForm.tsx` | `ToolMapField` uses the hook instead of deriving rows from the value. |
| `apps/elitea-web/src/pages/admin/ConfigurationGuardrailsToolMap.test.tsx` | New page-level tests through `AdminConfiguration` and `useAdminConfigurationPage`, asserting the PUT body. |

The stored value and the save contract are unchanged: blank rows are still never
serialised, and only changed keys are sent.

## Tests

`npx vitest run --config vitest.config.ts --project node`:

- New file: 5 tests. The first 4 were written first and all failed before the fix
  (no row appeared); the post-save test was added after review. All 5 pass.
  - Add a row to an empty map, type `mcp` / `echo`, save → PUT
    `{sensitive_tools: {mcp: ["echo"]}}`.
  - Add a row below stored `github`, type `atlassian` → order stays
    `github, atlassian`; PUT carries both.
  - Two blank rows are kept and not flagged; Discard removes the added rows.
  - After Save, rows show the stored map sorted, without the unnamed row.
  - `GitHub` typed into a new row beside stored `github` shows the duplicate
    warning.
- `src/pages/admin/Configuration*.test.tsx` and
  `ConfigurationToolMapEditor.test.tsx`: 7 files, 54 tests, 0 skipped, all pass.
- Full `src/pages/admin/`: 57 files, 664 tests. One unrelated test
  (`AdminNativeClientsEditor` "registers a client…") timed out at 5 s under the
  parallel run and passes alone (18/18). It does not touch these files.
- `oxlint` on `src/pages/admin/`: clean. `tsc --noEmit`: clean for these files.

## Performance

- Mechanism: one reference comparison per render (`apps/elitea-web/src/pages/admin/ConfigurationToolMapEditor.tsx:124`); rows are
  re-derived only on an external value change, so a keystroke no longer sorts
  the whole map (`apps/elitea-web/src/pages/admin/ConfigurationToolMapEditor.tsx:130-133`).
- Proving test: `apps/elitea-web/src/pages/admin/ConfigurationGuardrailsToolMap.test.tsx:67` and `:87` record exactly one PUT per save.
- Measured: one PUT per Save in the browser check below; no extra requests.

## Durability

- Mechanism: unchanged. The server value is the only durable record. The
  post-save refetch is a new value reference, so `apps/elitea-web/src/pages/admin/ConfigurationToolMapEditor.tsx:124` replaces local rows
  with the stored map (`apps/elitea-web/src/pages/admin/useAdminConfigurationPage.ts:187`
  clears the draft after the invalidating mutation settles).
- Proving test: `apps/elitea-web/src/pages/admin/ConfigurationGuardrailsToolMap.test.tsx:134`.
- Measured: browser reload after each save showed the stored rows; DB rows
  matched.

## Resilience

- Mechanism: any value the editor did not emit (Discard, load, refetch)
  replaces local rows (`apps/elitea-web/src/pages/admin/ConfigurationToolMapEditor.tsx:124-128`). Blank rows are never serialised
  (`apps/elitea-web/src/pages/admin/ConfigurationToolMapEditor.tsx:92-97`).
- Proving test: `apps/elitea-web/src/pages/admin/ConfigurationGuardrailsToolMap.test.tsx:115` (Discard), `apps/elitea-web/src/pages/admin/ConfigurationGuardrailsToolMap.test.tsx:134` (post-save).
- Measured: removing both rows and saving restored `{}` / `{}` in the DB.

## Security

- Applies: none of the `rules/security.md` categories change. No new route,
  parser, egress, identity, secret or dependency surface; the PUT body shape is
  unchanged (`apps/elitea-web/src/pages/admin/ConfigurationToolMapEditor.tsx:92-97`).
- Proving test: `apps/elitea-web/src/pages/admin/ConfigurationGuardrailsToolMap.test.tsx:67` asserts the exact PUT body (only the changed key).
- Measured: the browser check went through the stack's own Main with its own
  session, admin check and CSP header; `npm audit` reports the same 7
  pre-existing findings (no dependency change).

## Recovery guarantees

| Component × phase | Class | Note |
| --- | --- | --- |
| Web/browser × admin configuration edit (before Save) | L (pre-existing, by design) | Unsaved form edits are browser state and are lost on reload, as everywhere in the admin console. Not new. Code `apps/elitea-web/src/pages/admin/ConfigurationToolMapEditor.tsx:116-136`; test `apps/elitea-web/src/pages/admin/ConfigurationGuardrailsToolMap.test.tsx:115`. |
| Web/browser × Save | I | One PUT of the changed keys; re-sending the same map is idempotent. Unchanged by this PR. Code `apps/elitea-web/src/pages/admin/useAdminConfigurationPage.ts:183-196`; test `apps/elitea-web/src/pages/admin/ConfigurationGuardrailsToolMap.test.tsx:67`. |

## Real-browser evidence

- Stack: shared p5ids standalone stack (Main
  `elitea-main:current-nats-efa7213e803d-hotfix1140-p5ids-20261008-r2`), used
  with the user's approval.
- Web: this branch's admin bundle (`vite build --mode admin`), served locally
  on `p5ids.localhost:5191`. Index HTML was assembled the way Main's adminui
  handler does: Main's own per-session `admin_ui_config` script and CSP header
  were spliced into the local `index.html`. All `/api` calls went to the real
  Main. No response mocks. Signed in through the stack's OIDC mock as its test
  admin identity.
- Steps:
  1. Guardrails showed both maps empty (DB `{}` / `{}`).
  2. **Add toolkit — Sensitive Action Tools** → a row appeared; typed `mcp`,
     added tool `echo`; Save → `PUT …/guardrails` 200. DB
     `sensitive_tools = {"mcp": ["echo"]}`; `blocked_tools` untouched.
  3. Reload → the `mcp` / `echo` row is shown.
  4. **Add toolkit — Blocked Tools** → `github` / `delete_repo`; Save; reload →
     both rows shown; DB `blocked_tools = {"github": ["delete_repo"]}`.
  5. Removed both rows, Save, reload → "No toolkits are listed" for both; DB
     back to `{}` / `{}`.
- The stack image was not rebuilt or redeployed. A deployed-image check of the
  merged Web is a follow-up for the next rehearsal stack refresh.

## Fixtures

Created through the UI only; reverted through the UI. No database writes.

## Follow-ups

- Confirm on the next rehearsal stack built from `main` with this change.
- Review notes not changed here: Save is not blocked while two rows share a
  toolkit key (the editor warns, by its existing design); adding then removing
  a row still marks the form dirty, as before this change.
