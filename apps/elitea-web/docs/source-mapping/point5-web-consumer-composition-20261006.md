# Point5 Web consumer composition

This increment connects preserved Web callers to current recovery and ordered YAML foundations.
It does not enable graph or operator recovery admission.

## Source boundary

The integration baseline is `e79c277bdcd12fd08fc5d487d80438c5a059c74d`.
The reviewed saved snapshot is `b7f78424bc61786af1450321071edb412ad406e5`.
Its direct base is `f8a1499d128cfe226500bc33a0bbef1d465ec4fa`.
Its third parent is `8c1bcfd9d41a4828cf1ac956b2cef8d278c14ae9`.

The preservation inventory identifies missing caller amendments in that exact saved delta.
This increment uses selected per-file hunks and selected missing tests.
It does not apply a stash or restore generated output.

The private packet retains baseline hashes, saved delta bytes, final hashes, and direct check logs.
Its path is `/private/tmp/elitea-graph-point5-web-20261006/`.

## Behavioral mapping

| Preserved boundary | Current consumer | Result |
| --- | --- | --- |
| Strict versioned recovery receipt | `shared/lib/nodeRecovery.ts` and `features/chat-messages/lib/nodeRecoveryBinding.ts` | Retain parser validation and exact response/generation binding. Export the existing per-message selector. |
| Active original recovery display | `features/chat-messages/ui/chat-box/ApplicationAnswer.tsx` and `NodeRecoveryNotice.tsx` | Display the node and safe failure reason. Remove active loading animation while retaining execution ownership. |
| Original run timeline | `features/pipelines/lib/flow-editor/hooks/useRunEvent.ts` | Apply recovery only to the active root response and generation. Retain Stop state and clear node performing markers. |
| Paused run controls | `RunStateNode.tsx`, `RunStateDialog.tsx`, and `RunStateDialog.status.tsx` | Keep Stop for a recovery interrupt. Completed or stopped runs retain Delete behavior. |
| Authored static boundaries | `parsePipelineTraversal.helpers.ts` | Retain before/after labels on stored routes, including END. Preserve synthetic legacy branch behavior. |
| Explicit child inputs | `useFunctionInputMapping.ts` | Retain selected-child entries during a required default write. Preserve empty, null, and invalid stored descriptors for runtime validation. |
| Meaningful declaration order | `useIsPipelineYamlCodeDirty.ts` | Use the current ordered fingerprint. Detect state order changes while ignoring formatting changes. |
| Attachment state synchronization | `usePipelineAttachmentYamlSync.hooks.ts` | Use one source-based atomic edit. Preserve valid unsaved source, declaration order, and authored attachment descriptors. |
| Flow/YAML composition | `EditorPanel.tsx` | Use the existing atomic editor and parser. Preserve raw text during view changes and layout-only saves. |
| State rename | `StateDrawer.tsx` | Display raw descriptors through the existing projection. Pass rename metadata to the ordered serializer. |

## Immediate YAML mode change correction

The existing shared `CodeMirrorEditor` delays change notifications by 30 milliseconds.
Its unmount cleanup cancels a pending notification without committing it.
The previous `EditorPanel` parses stored text and unmounts the editor when the user selects Flow.
That sequence can lose a valid or invalid draft before its notification reaches the store.
This timing gap exists in the integration baseline and predates the caller composition.

`YamlCodeEditor` now forwards the existing typed shared editor ref.
`EditorPanel` reads the current CodeMirror document before changing mode.
It stores that exact draft and parses the same draft before unmount.
Invalid source remains stored, and the last valid flow document remains intact.
The shared debounce and dependency versions remain unchanged.

CodeMirror normalizes document line endings to LF.
When its current document matches the stored source after that normalization, the caller retains the original stored bytes.
The existing commented CRLF regression continues to enforce this unchanged-source behavior.

Two new regressions use the real shared editor and controlled timers.
Both edit its actual document, advance 29 milliseconds, and confirm that the store still contains the old source.
They then select Flow and verify the exact draft, parsed document, and source after returning to YAML.
Both fail on the previous caller implementation with a direct nonzero exit.

The correction changes `EditorPanel.tsx`, `EditorPanel.test.tsx`, `YamlCodeEditor.tsx`, and this mapping.
Its private packet retains their preimages, postimages, hashes, and direct receipts.
Its path is `/private/tmp/elitea-graph-point5-yaml-flush-20261006/`.
The earlier frozen packet remains unchanged.

The node notice uses current `bodySmall` typography instead of the saved obsolete `body2` variant.
It displays no activation ID, graph thread, raw error, credential, or operator action grant.
Its guidance directs the user to an operator and retains the option to stop the execution.
It makes no deployment-capability claim because the receipt carries no capability field.

The run dialog independently requires Interrupt status before a retained recovery flag can expose Stop.
This matches the run node's existing status guard.

Malformed receipts, stale responses, stale generations, and child ownership do not hide active processing.
History conversion displays only an active persisted original receipt.
Terminal responses do not retain the notice.

Explicit child mappings remain scoped to the selected Application toolkit.
The amendment does not substitute a sibling toolkit or repair invalid authored values.
Undeclared and foreign child fields remain excluded.

YAML edits retain raw descriptor fields and explicit null values.
Invalid or unsupported source remains editable and unchanged.
Automatic attachment entries use the current Rust descriptor's `type` and `value` fields.
Known undefined route fields remain omitted by the existing strict serializer.
Unrelated undefined values still refuse atomically.

## Existing static continuation

The current `staticPipelinePause.ts` parser and `chatStreamStaticPause.ts` reducer remain authoritative.
The current generated operation is `continueChatExecution` on the existing `continue_predict` path.
This increment does not restore the older generated operation name or add another route.

The selected saved transport test uses that current generated route through `useChatStreamTransport`.
It proves response identity, root terminal metadata, checkpoint-selector refusal, and 409/422 refusal without resubmission.
Existing static-control tests cover selected leaves, retained siblings, and restored Test scope.

Root owns the two versioned recovery/static Markdown contracts outside Web.
This increment edits neither OpenAPI nor generated clients.

## Verification and limits

The initial caller run fails seven tests across three files on the baseline implementation.
Those failures cover exact recovery pause projection, state declaration order, and terminal-route pause labels.
The final focused run passes 321 tests across 24 files with a direct zero exit.
Full Web typechecking, full strict lint, and selected strict lint pass with direct zero exits.

The immediate-mode correction passes 79 focused tests across six files with a direct zero exit.
Its full Web typecheck and full strict lint also pass with direct zero exits.
Tests use at most two workers, and strict lint uses two threads.
The baseline reproduction selects two tests; the runner excludes the other 16 through the test-name filter.
The final focused run contains no skipped tests.

Checks include current Code debug, first-draft Test input, reset, canvas controls, and ordinary answer behavior.
The separate History/static/store compatibility run passes 107 tests across eight files with a direct zero exit.
Test logs retain jsdom scrollTo diagnostics; no test skip or broader mock is introduced.
Local checks use Node v24.19.0, below the package's Node >=26 requirement.
Exact CI runtime acceptance remains unproved.

The saved `graphExtensionContracts.test.ts` remains excluded because its required Rust shaping producer files are absent.
This increment adds no browser, Docker, SQL, deployment, or runtime recovery evidence.
Root owns assembled runtime acceptance and all admission decisions.
Focused Web tests do not prove restart, recovery takeover, nested pause execution, or partial effect safety.
