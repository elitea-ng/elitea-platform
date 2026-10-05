# Static pause authoring parity

## Status

Private patch prepared on 2026-10-04. The UI admits authored static pauses that the current native compiler accepts.

The focused UI suite passes 110 tests. Focused lint and TypeScript checks pass. The frozen packet records exact baseline and postimage hashes.

These checks cover authoring, admission, and graph labels. They do not prove worker continuation, persistence, browser behavior, or deployment acceptance.

## Current source to new source

| Current behavior and source | New source and behavior |
| --- | --- |
| `EliteaUI/src/[fsd]/features/pipelines/flow-editor/ui/settings/CommonInterruptSettings.jsx:55-103` writes both pause lists and updates incident edges. | `apps/elitea-web/src/features/pipelines/ui/settings/CommonInterruptSettings.tsx` restores both handlers. It preserves other YAML fields and edge data. |
| The current web component disables both pause switches and hides stored entry and terminal pause values. | Both switches display stored pause values. They permit before-entry and after-terminal pauses. The explicit disabled setting still applies. |
| `graph/compiler.rs:187-217` requires a sequence and limits each pause list to 128 entries. | `graphAdmission.helpers.ts` rejects non-list values and lists with more than 128 entries. |
| `graph/compiler.rs:620-621,1971-1984` requires legal, unique identifiers that name stored nodes. | The existing `document.static-interrupts` rule now validates these requirements. It identifies each malformed, unknown, or duplicate entry. |
| `graph/compiler.rs:945-962` installs before and after gates. It combines explicit after pauses with implicit Printer pauses. | The UI accepts valid before and after lists, including explicit Printer pauses. The compiler remains unchanged. |
| Router default and HITL terminal edges lose after-pause labels during graph parsing. Plain END-transition edges also omit these labels. | `parsePipelineTraversal.helpers.ts` labels completed stored-node routes from the authored lists. It retains synthetic legacy branch edges and existing labels. |
| `graphAdmission.types.ts` describes a blanket native refusal. | Its rule comment now describes the bounded, unique stored-node contract. |

The SDK business reference is `elitea_sdk/runtime/langchain/langraph_agent.py:1264-1265,1608,1699-1725`. It reads authored lists and adds Printer after pauses.

The UI retains its existing node family, node identifier, transition, and state guards. No new node family enters this patch.

## Focused evidence

`CommonInterruptSettings.test.tsx` covers exact node IDs, both switches, preserved YAML fields, preserved edge data, neighboring pause labels, and disabled controls.

It also covers stored entry and terminal pauses, missing nodes, malformed field display, and structured output.

`graphAdmission.helpers.test.ts` covers both fields, valid lists, malformed IDs, unknown IDs, non-list values, non-string entries, duplicates, and the 128-entry limit.

The existing admission cases still run. Added cases keep unsupported node families rejected and admit a configured Printer pause.

`parsePipelineTraversal.helpers.test.ts` covers plain terminal edges, Router defaults, HITL terminal routes, and synthetic legacy branch edges.

The private harness uses the existing dependency runtime. It installs no packages. Its Vite cache stays in the private packet.

## Remaining acceptance

Run the owning worker checks and assembled Gate 5 acceptance after adoption. Verify actual before and after continuation in conversation and editor Test flows.

Capture node effects, response identity, execution generation, and pause occurrence. Verify Printer and dynamic HITL behavior with the same assembled runtime.

The private authoring checks run no Cargo command, image build, browser, Docker, Kubernetes, database, or credential operation.
