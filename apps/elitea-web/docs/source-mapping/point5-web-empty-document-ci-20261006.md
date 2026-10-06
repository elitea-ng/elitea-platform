# Point5 Web empty-document CI correction

## Source boundary

The reviewed source head is `9ae93a9eee2121beea76c79d24572ec0095d1563`.
The first PR1084 visual job runs 68 cases and reports two failures before screenshot comparison.
The failed cases are `pipeline-editor-authored-graph` and `pipeline-editor-node-admission`.
Both fail at `addNodeThroughMenu`, where the menu remains mounted after selecting the first LLM or Agent.
The other 66 cases pass.

The log is `/private/tmp/elitea-code-hydration-delivery-20261006/INITIAL_VISUAL_CI.log`.
The visual fixture's `EMPTY_GRAPH` is the empty string.
Root fetches the completed visual job on head `9ae93a9ee`.
It reports the same two failed cases and 66 passes.
The retained log is `LATEST_VISUAL_CI.log` in the same evidence folder.

## Failure producer

`AddNodeMenu` calls `onAddNode` before `handleClose`.
The node hook writes the first node through `EditorPanel` and the actual YAML store.
The Point5 caller uses the strict atomic document editor.
Its serializer reads `originalYaml` with the pinned YAML library's `load` function.
That function throws `expected a document, but the input is empty` for empty, whitespace, or comment-only input.
The synchronous exception prevents both the document write and the subsequent menu close.

The existing document parser already accepts no-document input as an empty mapping through `loadAll`.
Before Point5, `EditorPanel` calls `dumpYaml(next)` without the original source option.
The new caller composition exposes the existing serializer incompatibility.
This failure is a source defect, not a screenshot difference or baseline drift.

## Correction

`dumpYaml.helpers.ts` reads the original source with `loadAll`.
It defaults to an empty mapping only when no document exists.
Explicit null remains distinct from that default.
The existing original-byte comparison and strict output roundtrip comparison remain active.
Malformed or multiple original documents still fail.
No menu handler, authoring gate, visual test, timeout, snapshot threshold, or baseline changes.

## Verification and limits

Six regressions use the real AddNodeMenu, node-creation hook, and YAML store.
They cover empty, whitespace, and comment-only sources for the first LLM and Agent node.
They verify the stored node, entry point, YAML roundtrip, flow-node publication, and menu closure.
Separate serializer tests cover unchanged empty source, first edits, explicit null, and refusal guards.

The baseline run exits one with nine failed tests, 21 passed tests, and six recorded event errors.
Those event errors contain the exact pinned YAML exception and serializer stack.
The final focused run exits zero with 110 tests across nine files and no skips.
Full Web typechecking and full strict lint also exit zero.
The test run uses at most two workers, and lint uses two threads.
An initial test-only type error used a Playwright option in a Testing Library locator.
The corrected locator retains Testing Library's default exact string match.

The agent's checks are component and source receipts, not a full-app visual run or runtime recovery proof.
Root separately verifies both first-node actions in Chrome against the real rehearsal backend.
The source Web server runs on port 18086 and proxies the existing backend on port 18084.
Native editor input clears an isolated pipeline draft. Adding the first LLM or Agent closes the menu and publishes the node.
The draft is neither saved nor executed. Reload restores the exact saved YAML for pipeline 144, version 168.
`ROOT_UI_RECEIPT.json` records the actions and screenshot hashes in the frozen correction packet.
This browser proof does not replace exact-head CI or deployed Worker recovery acceptance.
Local Node v24.19.0 remains below the Node >=26 package requirement.
Root owns the next exact-head CI visual result.

The frozen correction packet is `/private/tmp/elitea-graph-point5-empty-yaml-ci-20261006/`.
It retains reviewed preimages, final hashes, the red receipt, and direct check exits.
All earlier frozen packets remain unchanged.
