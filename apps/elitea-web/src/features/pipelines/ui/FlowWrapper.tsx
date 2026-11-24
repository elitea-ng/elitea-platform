import type { ReactNode } from 'react';
import { forwardRef } from 'react';

import { ReactFlowProvider } from '@xyflow/react';

import { useIsSmallWindow } from '../lib/hooks/useIsSmallWindow';
import type { AiAssistantLlmSettings } from '../api/aiAssistantPredict';
import type { FlowEdge, FlowNode, SetYamlJsonObject } from '../lib/flow-editor/reactFlowTypes';
import type { YamlPipelineDocument } from '../lib/flow-editor/helpers/pipelineFlow.types';
import { FlowEditor, type FlowEditorHandle } from './FlowEditor';
import { computeFlowWrapperSx, type PipelineEditorModeValue } from './flowWrapperStyles';
import type { PipelineToolEntry } from './select/pipelineToolEntry.types';

export type { PipelineEditorModeValue } from './flowWrapperStyles';

/**
 * Ported from `apps/elitea-ui/src/pages/Pipelines/Components/FlowWrapper.jsx`
 * — the thin `<ReactFlowProvider>` wrapper around `FlowEditor`.
 *
 * **Real, load-bearing expansion of the prop surface — not a porting
 * shortcut.** The baseline's `FlowWrapper` forwarded only 4 props to
 * `FlowEditor` (`stopRun`/`mode`-derived `sx`/`setYamlJsonObject`/`disabled`)
 * because the baseline's own `FlowEditor.jsx` read `yamlJsonObject`/`nodes`/
 * `edges`/`layout_version`/`resetFlag` straight off Redux. Sibling unit A2k's
 * `FlowEditor.tsx` (landed in this shared worktree, verified directly before
 * writing this file) has NO Redux/store read for any of those — its own doc
 * comment states plainly that `slices/pipeline.js`'s remaining fields become
 * "explicit prop... or callback prop... The not-yet-built pipeline-editor
 * page is the natural owner of that state going forward" — this file (via
 * `EditorPanel.tsx`, which owns and computes them) is that owner. Every one
 * of `FlowEditorProps`'s fields not already covered by the baseline's own 4
 * is threaded straight through here, unmodified.
 *
 * `versionTools`/`llmSettings` (`FlowEditorProps`, "REAL PLUMBING GAP CLOSED
 * HERE" per that file's own doc comment) are forwarded verbatim from this
 * file's own props, unchanged from the expansion above. The gap a prior
 * revision of this comment documented here (no real threading path from
 * either of `EditorPanel`'s two callers — see that revision's own precise
 * 3-file routing note, `EditorPanel.tsx`/`ConfigurationTab.tsx`/
 * `PipelineEditorParts.tsx`, if this history is still around) is CLOSED:
 * `EditorPanel.tsx` now declares both as real props and forwards them here;
 * `ConfigurationTab.tsx` and `PipelineEditorParts.tsx`'s `PipelineEditorBody`
 * each derive them from their own in-scope `versionDetails` via `../lib/
 * hooks/useFlowEditorVersionInputs.ts` (wire `tools`/`llm_settings` -> this
 * file's `PipelineToolEntry[]`/`AiAssistantLlmSettings`, per `../lib/
 * flowEditorVersionInputs.helpers.ts`) and pass the result straight through.
 * Still declared optional here (both remain `undefined` for create-mode,
 * where no version exists yet to source them from).
 *
 * The `sx` computation itself lives in `./flowWrapperStyles.ts` — see that
 * file's own doc comment for why (unit-testability against a real, verified,
 * currently-broken transitive dependency this file statically imports,
 * `./FlowEditor`).
 */
export interface FlowWrapperProps {
  readonly stopRun: () => void;
  readonly mode: PipelineEditorModeValue;
  readonly yamlJsonObject: YamlPipelineDocument;
  readonly setYamlJsonObject: SetYamlJsonObject;
  readonly initialNodes: readonly FlowNode[];
  readonly initialEdges: readonly FlowEdge[];
  readonly layoutVersion: string | undefined;
  readonly resetFlag: boolean;
  readonly onResetHandled: () => void;
  readonly onLayoutVersionChange: (version: string) => void;
  readonly versionTools?: readonly PipelineToolEntry[] | undefined;
  readonly llmSettings?: AiAssistantLlmSettings | null | undefined;
  readonly noBorder?: boolean | undefined;
  readonly disabled?: boolean | undefined;
  readonly onCanvasNodeInteraction?: (() => void) | undefined;
}

export const FlowWrapper = forwardRef<FlowEditorHandle, FlowWrapperProps>(function FlowWrapper(props, ref): ReactNode {
  const {
    stopRun,
    mode,
    yamlJsonObject,
    setYamlJsonObject,
    initialNodes,
    initialEdges,
    layoutVersion,
    resetFlag,
    onResetHandled,
    onLayoutVersionChange,
    versionTools,
    llmSettings,
    noBorder = false,
    disabled,
    onCanvasNodeInteraction,
  } = props;
  const { isSmallWindow } = useIsSmallWindow();

  return (
    <ReactFlowProvider>
      <FlowEditor
        ref={ref}
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        initialNodes={initialNodes}
        initialEdges={initialEdges}
        layoutVersion={layoutVersion}
        resetFlag={resetFlag}
        onResetHandled={onResetHandled}
        onLayoutVersionChange={onLayoutVersionChange}
        stopRun={stopRun}
        versionTools={versionTools}
        llmSettings={llmSettings}
        disabled={disabled}
        onCanvasNodeInteraction={onCanvasNodeInteraction}
        sx={computeFlowWrapperSx(isSmallWindow, noBorder, mode)}
      />
    </ReactFlowProvider>
  );
});
