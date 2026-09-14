# Chat persona compatibility

## Source mapping

Current SDK paths start at `projects/elitea-sdk/elitea_sdk/runtime/langchain`.
New platform paths start at this repository root.

| Current source | New source | Behavior |
| --- | --- | --- |
| `assistant.py::_prepare_prompt` selects ad-hoc persona templates. | `services/elitea-worker-rust/src/agents/assembly.rs::adhoc_persona_instructions` | Ad-hoc chat applies the selected response style without replacing project instructions. |
| `constants.py` defines QA, nerdy, quirky, and cynical personas. | The same Rust helper defines concise behavioral instructions. | Preserve the style and purpose. Do not copy Python prompt assembly or tool directives. |
| `assistant.py` keeps saved-agent instructions separate from persona templates. | `assembly.rs::application_model_for_agent_type` | Saved agents retain their authored instructions. |
| Core conversation metadata stores the persona. | Main `internal/application/agentexecution/adhoc.go::currentAdhocConversationOptions` | Freeze the selected persona into the execution input. |

Generic and none retain the existing default instructions.
Bare adds no persona instruction.
Tool permissions, project context, and skills retain their existing authority paths.

## Failure observed on 2026-09-09

An existing chat sends `persona=cynical` with a simple greeting.
Rust rejects this value before model invocation and publishes `UNSUPPORTED_CAPABILITY`.
The UI displays `Configuration type is not supported.`
The rejection originates in commit `c15f9f049`, before the latest main merge.
This is a missing runtime behavior, not a vault decryption failure.

The regression test covers all four styles and preserves saved-agent instructions.
It also rejects an unknown persona.
Deployed response and continuation checks remain required.

## Focused verification

The Rust assembly suite passes: 27 tests, zero failures, zero skips.
The running Main master key matches the source Centry Main master key.
The comparison runs in memory and returns only a boolean result.
Neither the key nor its hash enters this document.
The persona image builds successfully and passes Clippy with warnings denied.
The rehearsal worker now runs the repair.
The same cynical-persona chat passes admission and reaches model setup.
Its enabled internal tools encounter a separate gateway rejection; successful response verification remains open.

After the internal schema repair, the same chat returns a successful greeting through the Rust worker.
Its cynical persona and all internal MCP groups remain enabled.
The gateway rejection no longer blocks this greeting.
