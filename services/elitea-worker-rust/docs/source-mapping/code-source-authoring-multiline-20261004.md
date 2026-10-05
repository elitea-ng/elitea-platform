# Code source authoring

Date: 2026-10-04. Status: source and focused UI checks pass.
Product browser acceptance remains with the rollout owner.

## Current source to target ownership

| Current source | Target owner | Observable contract |
| --- | --- | --- |
| Legacy `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/settings/NodeFieldInput.jsx` | `apps/elitea-web/src/features/pipelines/ui/settings/SimpleLLMInputItem.tsx::NodeFieldInput` | Fixed and template Code sources use a multiline control. |
| Legacy `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/settings/SimpleLLMInputItem.jsx` | `SimpleLLMInputItem.tsx::resolveCodeFieldLanguage` | Code fields use the selected language. Omitted language retains Python. |
| Existing `CodeNode.tsx` language selector and `SimpleLLMInputs.tsx` input rows | `CodeNode.tsx`, `SimpleLLMInputs.tsx`, `SimpleLLMInputItem.tsx` | The selected language reaches the Code field and AI editor. |
| Existing `AIAssistantInput.tsx` inline `InputBase` | `apps/elitea-web/src/features/pipelines/ui/AIAssistantInput.tsx` | Code fields keep newlines when AI streaming capability is enabled. |
| Existing `shared/ui/StyledInputEnhancer` full-screen editor | Existing shared input control | Code source edits use the same value and change handler in both views. |
| Existing `useCodeInputMapping.ts` mapping update | Existing Code mapping owner | The edit changes the Code value and retains its mapping type, language, and sibling nodes. |

## Source loss and correction

The previous inline Code control renders a native single-line input.
That control removes LF characters from its DOM value.
An edit can then replace multiline source with collapsed source.
The legacy plain branch has the same Code multiline gap.

Fixed and template Code fields now enable the existing multiline input option.
The plain control also exposes the existing expansion and full-screen actions.
The AI control enables multiline input from its existing Code field name.
The selected Code language reaches the AI editor through an optional row setting.
Python, JavaScript, TypeScript, and Rust retain their selected content type.
Ordinary task fields retain their single-line control.

The amendment changes no source resolver, execution authority, dependency, or runtime profile.
The amendment adds no syntax grammar package or syntax highlighting.
The existing AI modal and Code mapping update retain their current contracts.

## Focused fixtures

`SimpleLLMInputItem.test.tsx` checks fixed and template source with AI capability enabled and disabled.
Each case checks the textarea value, edit callback, and reopened field.
The fixtures retain LF characters, indentation, blank lines, and a trailing newline.
The existing full-screen path also retains tab indentation and the edited source after reopening.
The selected-language cases open the AI editor for all four Code languages.
Two ordinary task cases retain a native single-line input with AI capability enabled and disabled.

`CodeNode.test.tsx` checks all four Code languages through the actual Code mapping hook.
Each case edits source, serializes YAML, parses YAML, and reopens the Code card.
The fixture retains the selected language, input mapping, output, transition, and sibling node.
These checks prove the tested LF source strings and indentation.
They do not establish browser handling of other line-ending encodings.

## Implementation history and verification

2026-10-04: Browser acceptance identifies newline removal in the inline Code input.
The private baseline reproduction reports 12 failures, 37 passes, and one existing expected Debug-toggle failure.
The amendment changes four UI source files and two focused test files.
The source mapping records those changes.

The final focused run covers `SimpleLLMInputItem`, `CodeNode`, `AIAssistantInput`, and `SimpleLLMInputs`.
It reports 55 passes and one existing expected Debug-toggle failure.
Focused lint and the application TypeScript check pass.
All three final checks use explicit Node 24.19.0 and existing locked dependencies.
The private manifest pins source baselines, final source files, patch bytes, and verification logs.

The checks use local UI fixtures and mocked API responses.
They perform no browser, build, deployment, container, database, or Code execution operation.
The rollout owner retains product acceptance and deployment verification.
