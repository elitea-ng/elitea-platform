# Bounded Code source mappings

Date: 2026-10-04. This feature supplies Code source resolution.
It does not change execution authority or public protocols.

## Source mapping

| Current SDK behavior | Rust owner | Result |
| --- | --- | --- |
| `runtime/langchain/langraph_agent.py:1413-1436` passes Code through `FunctionTool.input_mapping`. | `src/agents/graph/code.rs::CodeSource` | Preserve fixed and variable mappings. Add typed `fstring`. |
| `runtime/langchain/utils.py:529-560` applies Python string formatting, then a state fallback. | `code.rs::visit_template` | Support named fields and escaped braces. Reject unsupported fields and malformed braces. |
| `create_params` and `safe_format` convert selected or fallback state to text. | `code.rs::resolve_template_variable` | Preserve string bytes and native signed/unsigned integer decimal text. |
| Missing SDK fields can survive as unresolved placeholders. | `code.rs::resolve_source` | Refuse missing fields before Code runtime effects. Do not retain unresolved executable fields. |
| The legacy Code path can reach runtime state. | `code.rs::validate_source_types` | Permit declared user references and built-in `input`. Exclude messages and reserved runtime keys. |
| Existing fixed source has no template expansion. | `code.rs::config_digest`, `code_tests.rs` | Preserve existing serialized bytes and configuration digests. |
| The graph owns Code activation and selected state. | `code_runtime.rs::CodeNode` | Validate references during binding. Resolve exact bytes before the runtime call. |

The current SDK reference HEAD is `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`.
This implementation uses that behavior reference. It does not execute Python expressions during preparation.

## Bounded template contract

Code YAML uses `code: {type: fstring, value: ...}`.
A field uses `{name}` with an ASCII letter or underscore first.
Subsequent characters can include ASCII digits. Each name has a 256-byte limit.
Use `{{` and `}}` for literal braces.
Permit at most 256 field occurrences, including repeated fields.
Keep the existing 256 KiB source limit for both templates and rendered source.
Check each expansion length before copying state text.

References require declared `str` or `int` state. Built-in `input` remains a string.
Strings preserve exact UTF-8 bytes, newlines, and quotes. Source interpolation does not quote or escape a replacement.
Signed and unsigned native integers use exact decimal text, matching SDK `str(int)`.
The declared value must retain its type. No string-to-integer or boolean coercion occurs.
Code input selection controls state export. It does not remove declared references from source mapping.
This preserves the SDK fallback lookup for permitted user state.

Refuse missing references, reserved fields, null bytes, wrong values, and empty final source.
Refuse positional fields, attribute access, indexing, expressions, conversion flags, and format specifications.
Errors contain no source, state values, or private field names.

Float, boolean, list, and dictionary field formatting remains unsupported.
Python float formatting differs from JSON number spelling and Rust display formatting.
This slice adds no replacement numeric formatter or Python object representation contract.
Typed Code state inputs and output variables still support their existing types.
Users can supply an explicit string for source text instead of relying on coercion.

## Resolved source and authority interface

`ResolvedCode` has private immutable bytes and provenance.
`source()` borrows the exact resolved bytes. `provenance()` returns `CodeProvenance`.
Fixed source retains `SavedLiteral`. Variable source retains `StateVariable`.
Every fstring mapping uses `StateTemplate`, including a template without fields.
Equal final source does not change the mapping's provenance or definition identity.

`CodeInvocation.source` remains `&str` through the runtime await.
The resolved value owns template bytes for that complete call.
The existing activation still binds graph thread, node, step, and frozen definition digest.
The execution request still fingerprints actual source, selected input, language, image, policy, and dependency identity.
The separate authority owner must prove these bindings before admitting dynamic source.
This packet leaves `code_remote.rs` unchanged and retains its existing refusal.

## Initial verification boundary

Eleven new mapping fixtures cover syntax, provenance, immutable configuration, declarations, integer text, and byte limits.
Three new runtime fixtures exercise actual graph compilation, typed template resolution, and refusal before runtime calls.
The positive runtime fixture uses a test double. It does not execute sandbox code.
Existing fixed-source digest fixtures remain unchanged.

Standalone rustfmt checks pass for the three private Rust files.
Cargo tests and strict Clippy await the root-owned build window.
No deployed, browser, database, Docker, or Kubernetes acceptance occurs during this slice.

## Adopted source and product acceptance

The root adopts the reviewed mapper and separate dynamic-authority amendment.
The exact Code candidate passes 1,671 tests, with zero failures and 55 ignored integrations.
Strict all-target, all-feature Clippy and formatting checks pass.
These checks precede the later Parallel integration and do not verify its additions.

The [dynamic authority mapping](code-dynamic-source-authority-20261004.md) records the separate remote admission contract.
The [product acceptance](code-source-ui-recovery-acceptance-20261004.md) records saved source, typed state, and exact durable identities.
Variable, template, and fixed Python nodes execute through Main-authorized Docker admission.
Invalid variable and template sources create no execution job or later-node effect.
Worker loss resumes the original jobs and returns one persistent chat result after reload.
Kubernetes, compilation-cache, and other Code release gates remain open.
