# Project Context save cache consistency

## User outcome

A successful Save keeps the committed Project Context in the editor while its background GET is pending.
A refused Markdown import leaves that content unchanged.
Discard also restores the committed content during the same pending GET.

## Source mapping

| Existing source | Existing behavior | Corrected behavior |
| --- | --- | --- |
| `apps/elitea-web/src/pages/settings/ProjectContext.tsx`, `saveMutation.onSuccess` | Starts query invalidation without waiting for its GET. | Installs the successful PUT response before starting the existing invalidation. |
| The same file, `handleSave` | Clears `isDirty` after the PUT succeeds. | Preserves this save lifecycle. The query already contains the committed response. |
| The same file, server-data synchronization effect | Copies query content when `isDirty` becomes false. | Copies the committed response while the refreshed GET remains pending. |
| The same file, `handleDiscard` | Restores the current query body. | Restores the committed response during the pending GET. |
| `apps/elitea-web/src/shared/api/generated/applications/applications.ts` | GET and PUT return the same Project Context response shape. | Uses the generated GET query key without changing generated code. |
| `apps/elitea-web/src/features/settings/lib/project-context/readMarkdownImport.ts` | Reports overlong imports without calling `onText`. | Preserves this existing refusal behavior. |

The save callback previously exposed old query data after clearing the dirty guard.
The controlled test holds the refreshed GET after a successful PUT.
The old component then resets its parent content state to empty.
Its character counter reports 2500 remaining characters instead of 2456.
CodeMirror can briefly retain its old document before synchronizing with that empty parent state.
The refused import does not cause the reset. The save synchronization race causes it.

## Verification

The new paired test uses the real component, CodeMirror, React Query, generated HTTP client, and MSW handlers.
The test controls GET completion with a promise. It uses no timing sleeps.
The baseline fails before the production fix. The corrected paired file passes all 11 tests with zero skips.
The regression also verifies the successful PUT body, import refusal, parent character counter, and Discard during the pending GET.
Existing toggle, unsaved-edit, permission, and Markdown-import tests remain active.
Focused lint and full Web typecheck pass.

Root's isolated browser probe passes in Chromium and WebKit with the production component, router, editor, and HTTP/query lifecycle.
Both probes hold the post-save GET and refuse imports of 2501 and 7500 characters without changing the saved buffer.
Each probe records one exact PUT, two GETs, and zero unknown requests, page errors, or console errors.
The fixture supplies full generated Project Context and permission responses.
The private verification packet retains the typed requests, browser traces, and final screenshots.

No E2E assertion, timeout, retry, skip, permission, dependency, or backend contract changes.
Local checks use Node v24.19.0. The application declares Node 26 or newer.
The browser boundary uses an isolated typed HTTP fixture. It does not include AppShell project switching or a live backend.
The full owning CI journey remains separate. Root reviews both final screenshots.
These checks do not prove deployed behavior or backend persistence.
