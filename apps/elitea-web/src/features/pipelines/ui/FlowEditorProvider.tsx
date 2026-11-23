/**
 * `<FlowEditorContext.Provider>` JSX. `../lib/flow-editor/flowEditorContext.
 * ts`'s own doc comment (unit A2d) stops at the bare `createContext` call
 * and explicitly defers the JSX Provider to "whichever pipelines sub-unit
 * owns `features/pipelines/ui/**`" — that is this sub-unit (A2k), since
 * `FlowEditor.tsx` (same sub-unit) is `FlowEditorContext`'s only consumer
 * that needs a Provider wired up (every `ui/nodes/*` reader just calls
 * `useContext(FlowEditorContext)`).
 *
 * Ported from `apps/elitea-ui/src/[fsd]/app/providers/FlowEditorProvider.
 * jsx` (49 lines) verbatim, field-for-field. NOT placed under `src/app/` —
 * that location is layer-illegal for a `features/pipelines`-owned context
 * per R-L1 (`app` sits above `features` in the layer model, spec §3.2);
 * same relocation `flowEditorContext.ts` already made for the context
 * object itself.
 *
 * **Budget-forced deviation (§3.5 `hookDeps` ≤ 8):** the baseline memoises
 * `contextValue` with `useMemo` over an 11-entry dependency array
 * (`FlowEditorProvider.jsx:21-47`) — one entry per context field. This
 * slice's `hookDeps` budget caps any hook's dependency array at 8, so the
 * `useMemo` is dropped outright rather than truncating the array (which
 * would silently stop re-deriving `contextValue` when a dropped field
 * changes — a real bug, not a style nit). The object is built fresh every
 * render instead: cheap (11 field copies, no computation), and every
 * consumer already reads it through `useContext`, which re-renders on any
 * new object reference regardless of whether the *fields* changed — same
 * downstream behaviour, one avoidable render skipped less often.
 */
import type { ReactNode } from 'react';

import { FlowEditorContext, type FlowEditorContextValue } from '../lib/flow-editor/flowEditorContext';

/** @public `FlowEditor.tsx`'s own composition — not re-exported from this slice's `index.ts` (canvas-internal plumbing, not a public surface). */
export interface FlowEditorProviderProps extends FlowEditorContextValue {
  readonly children: ReactNode;
}

export function FlowEditorProvider(props: FlowEditorProviderProps): ReactNode {
  const {
    children,
    editorHeight,
    editorWidth,
    yamlJsonObject,
    setYamlJsonObject,
    deleteRunNode,
    isRunningPipeline,
    handleDeleteNode,
    handleDuplicateNode,
    expandAll,
    setFlowNodes,
    setFlowEdges,
    disabled,
  } = props;

  // `FlowEditorContextValue` (A2d, not owned here) declares
  // `editorHeight`/`editorWidth`/`deleteRunNode`/`isRunningPipeline`/
  // `handleDeleteNode`/`expandAll`/`disabled` as plain `?: T` (optional)
  // rather than `?: T | undefined` (optional AND explicit-undefined) --
  // under this repo's `exactOptionalPropertyTypes: true`, that makes
  // EXPLICITLY setting any of those keys to `undefined` (which every real
  // caller of this component legitimately can, e.g. `disabled` genuinely
  // unset) a type error, even though it is runtime-identical to omitting
  // the key: every real consumer (`useContext(FlowEditorContext)?.field`,
  // `AgentNode.tsx`/`CodeNode.tsx`/etc.) reads every one of these fields
  // through optional chaining, which treats "key absent" and "key present
  // as `undefined`" identically. One documented cast here, rather than
  // fighting the stricter-than-necessary sibling type field-by-field.
  const contextValue = {
    editorHeight,
    editorWidth,
    yamlJsonObject,
    setYamlJsonObject,
    deleteRunNode,
    isRunningPipeline,
    handleDeleteNode,
    handleDuplicateNode,
    expandAll,
    setFlowNodes,
    setFlowEdges,
    disabled,
  } as FlowEditorContextValue;

  return <FlowEditorContext.Provider value={contextValue}>{children}</FlowEditorContext.Provider>;
}
