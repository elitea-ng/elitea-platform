/**
 * `FlowEditor.jsx`'s seven top-level `useEffect` calls (baseline lines
 * 116-118, 177-197, 199-203, 432-446, 448-455, 457-464, 466-473, 475-481),
 * split into six small hooks — one component with 7 `useEffect`s blows
 * past §3.5's `useEffectsPerComponent` budget of 3 by more than double.
 * Grouped in this one file (rather than one file per hook) because every
 * one of them is single-purpose, `FlowEditor.tsx`-private plumbing with no
 * reuse case elsewhere in the slice — matching this batch's own precedent
 * (`useDeleteItems.ts`'s local `useDeleteKeyTrigger`) of splitting a big
 * hook's effects into smaller named functions rather than files.
 *
 * DISCLOSED REDESIGN (this sub-unit, A2k): the baseline reads
 * `state.pipeline`'s `nodes`/`edges`/`layout_version`/`resetFlag` and
 * dispatches `actions.clearResetFlag()`/`actions.setLayoutVersion(...)` via
 * Redux (`useSelector`/`useDispatch`, `FlowEditor.jsx:3,40,96-101,113-114,
 * 178-197,475-481`). This app has no Redux (zustand only,
 * `package.json`) — `pipelineEditorStore.ts`'s own doc comment explicitly
 * scopes itself to `{nodes, edges}`/`stateValidationErrors` only and defers
 * the REST of `slices/pipeline.js` ("`yamlJsonObject`/`yamlCode`/
 * `orientation`/`resetFlag`/`initState`") to "whichever sub-unit owns the
 * pipeline-editor PAGE/composition root, scoped to what it actually
 * needs" — that is `FlowEditor.tsx`, the top-level canvas composition root
 * (this same sub-unit). Every one of those reads/dispatches becomes an
 * explicit prop (`initialNodes`/`initialEdges`/`layoutVersion`/`resetFlag`)
 * or callback prop (`onResetHandled`/`onLayoutVersionChange`) instead, the
 * exact same "ambient Redux -> explicit prop, caller supplies it" swap
 * every other Redux-touching hook in this slice already made
 * (`useSaveNodeAndEdges.ts`, `useStateValidation.ts`). The real Redux `{
 * nodes, edges }` CACHE (`state.pipelineEditor`, `FlowEditor.jsx:94,103,
 * 128-129`) is still read from `pipelineEditorStore` unchanged — that half
 * of the split already had a zustand home.
 */
import { useEffect, useRef, useState, type RefObject } from 'react';

import type { SxProps, Theme } from '@mui/material/styles';

import type { YamlPipelineDocument } from '../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowEdge, FlowNode, SetFlowEdges, SetFlowNodes, YamlPipelineDocumentRef } from '../lib/flow-editor/reactFlowTypes';

/** `FlowEditor.jsx:102,116-118` — mirrors `yamlJsonObject` into a ref every render so callback closures (`onConnect`, `useIncompleteEdge`) always read the latest value without being re-created. */
export function useYamlJsonObjectRef(yamlJsonObject: YamlPipelineDocument): YamlPipelineDocumentRef {
  const ref = useRef<YamlPipelineDocument>(yamlJsonObject);
  useEffect(() => {
    ref.current = yamlJsonObject;
  }, [yamlJsonObject]);
  return ref;
}

/**
 * Generic "always-current" ref — mirrors `value` on every render (no
 * `useEffect`, so the ref is up to date even mid-render, before the next
 * commit). Used by `FlowEditor.tsx`'s `useImperativeHandle` call: the
 * baseline's own equivalent dep array has 11 entries (`FlowEditor.jsx:350-
 * 361`), well past §3.5's `hookDeps` budget of 8. Reading every value
 * through one `useLatest` ref instead lets the imperative handle keep an
 * EMPTY dependency array (the handle's own methods never go stale — they
 * dereference `ref.current` at call time) while preserving the exact same
 * "always reflects the latest closures" guarantee the 11-entry array was
 * itself only approximating.
 */
export function useLatest<T>(value: T): { readonly current: T } {
  const ref = useRef(value);
  ref.current = value;
  return ref;
}

/**
 * `sx?.display` (`FlowEditor.jsx:86,157,200`'s `display: sx?.display`
 * reads, threaded into `useDeleteItems`/`useCtrlASelectAll` so Delete-key/
 * Ctrl+A handling is suppressed while the canvas itself is hidden, e.g.
 * `FlowWrapper.jsx`'s own `display: mode === Flow ? undefined : 'none'`).
 * `sx` here is `SxProps<Theme>` — a plain object, a theme callback, or an
 * array of either — so the read has to narrow to the plain-object case
 * first; a callback/array `sx` (`FlowEditor` never receives one from any
 * real caller today) yields `undefined`, same as the baseline's own
 * `sx?.display` would on anything but a plain object.
 */
export function extractSxDisplay(sx: SxProps<Theme> | undefined): string | undefined {
  if (sx === undefined || Array.isArray(sx) || typeof sx === 'function') return undefined;
  const value = (sx as Record<string, unknown>).display;
  return typeof value === 'string' ? value : undefined;
}

export interface UseFlowEditorResetArgs {
  readonly resetFlag: boolean;
  readonly initialNodes: readonly FlowNode[];
  readonly initialEdges: readonly FlowEdge[];
  readonly setFlowNodes: SetFlowNodes;
  readonly setFlowEdges: SetFlowEdges;
  readonly onResetRunParseStatus: () => void;
  readonly onResetHandled: () => void;
  readonly persistNodes: (nodes: readonly FlowNode[]) => void;
  readonly persistEdges: (edges: readonly FlowEdge[]) => void;
  readonly fitView: () => void;
}

/** `FlowEditor.jsx:177-197` — snap the canvas back to `initialNodes`/`initialEdges` when the caller flips `resetFlag`, then re-sync the persisted copy and re-fit the viewport once React has measured the reset nodes. */
export function useFlowEditorReset(args: UseFlowEditorResetArgs): void {
  const { resetFlag, initialNodes, initialEdges, setFlowNodes, setFlowEdges, onResetRunParseStatus, onResetHandled, persistNodes, persistEdges, fitView } = args;

  useEffect(() => {
    if (!resetFlag) return;

    setFlowNodes([...initialNodes]);
    setFlowEdges([...initialEdges]);
    onResetRunParseStatus();

    // Force sync nodes to the persisted copy after reset so measured
    // heights are available for save — without this the persisted copy
    // stays empty because `flowNodes === initialNodes` (baseline comment,
    // `FlowEditor.jsx:184-185`, preserved verbatim).
    const timer = setTimeout(() => {
      persistNodes(initialNodes);
      persistEdges(initialEdges);
      const hasAuthoredNode = initialNodes.some(node => node.type !== 'entry_point' && node.type !== 'END');
      if (initialNodes.length > 2 || hasAuthoredNode) fitView();
      onResetHandled();
    }, 150);
    return () => clearTimeout(timer);
    // baseline disables exhaustive-deps here too (`FlowEditor.jsx:196`) — only re-run when the reset flag or the reset target itself changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [resetFlag, initialNodes, initialEdges]);
}

export interface FlowEditorResizeState {
  readonly editorRef: RefObject<HTMLDivElement | null>;
  readonly editorHeight: number;
  readonly editorWidth: number;
}

/** `FlowEditor.jsx:111-112,432-446` — tracks the canvas container's own measured size via `ResizeObserver`, seeded with the baseline's literal initial fallback (`622x677`, used until the first observed resize). */
export function useFlowEditorResizeObserver(): FlowEditorResizeState {
  const editorRef = useRef<HTMLDivElement | null>(null);
  const [editorWidth, setEditorWidth] = useState(622);
  const [editorHeight, setEditorHeight] = useState(677);

  useEffect(() => {
    if (!('ResizeObserver' in window) || !editorRef.current) return undefined;
    const resizeObserver = new ResizeObserver(entries => {
      for (const entry of entries) {
        // `ResizeObserverEntry.target` is typed as the DOM-spec's plain
        // `Element` — narrowed back to `HTMLElement` for `.offsetHeight`/
        // `.offsetWidth` (`FlowEditor.jsx:437-438`'s exact reads), safe
        // because the only element ever observed is `editorRef.current`,
        // an `HTMLDivElement`.
        const target = entry.target as HTMLElement;
        setEditorHeight(target.offsetHeight);
        setEditorWidth(target.offsetWidth);
      }
    });
    resizeObserver.observe(editorRef.current);
    return () => resizeObserver.disconnect();
  }, []);

  return { editorRef, editorHeight, editorWidth };
}

/** `FlowEditor.jsx:448-455` — fit the viewport once on mount when the initial graph already has more than the bare entry/end pair. */
export function useFlowEditorInitialFitView(initialNodesLength: number, fitView: () => void): void {
  useEffect(() => {
    if (initialNodesLength <= 2) return undefined;
    const timer = setTimeout(fitView, 100);
    return () => clearTimeout(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initialNodesLength]);
}

export interface UseFlowEditorPersistenceArgs {
  readonly flowNodes: readonly FlowNode[];
  readonly flowEdges: readonly FlowEdge[];
  readonly initialNodes: readonly FlowNode[];
  readonly initialEdges: readonly FlowEdge[];
  readonly isRunningPipeline: boolean;
  readonly persistNodes: (nodes: readonly FlowNode[]) => void;
  readonly persistEdges: (edges: readonly FlowEdge[]) => void;
}

/** `FlowEditor.jsx:457-473` — mirror the live canvas `flowNodes`/`flowEdges` into the persisted copy (`pipelineEditorStore`) whenever they diverge from the last-known-saved graph, debounced one macrotask like the baseline. */
export function useFlowEditorPersistence(args: UseFlowEditorPersistenceArgs): void {
  const { flowNodes, flowEdges, initialNodes, initialEdges, isRunningPipeline, persistNodes, persistEdges } = args;

  useEffect(() => {
    if (JSON.stringify(flowNodes) === JSON.stringify(initialNodes) || isRunningPipeline) return undefined;
    const timer = setTimeout(() => persistNodes(flowNodes), 100);
    return () => clearTimeout(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [flowNodes]);

  useEffect(() => {
    if (JSON.stringify(flowEdges) === JSON.stringify(initialEdges)) return undefined;
    const timer = setTimeout(() => persistEdges(flowEdges), 100);
    return () => clearTimeout(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [flowEdges]);
}

export interface UseFlowEditorLayoutVersionArgs {
  readonly layoutVersion: string | undefined;
  readonly currentLayoutVersion: string;
  readonly nodesInitialized: boolean;
  readonly onReLayout: () => void;
  readonly onLayoutVersionChange: (version: string) => void;
}

/** `FlowEditor.jsx:475-481` — one-time auto-relayout migration: re-lay-out and bump the stored layout version whenever it lags the app's current layout algorithm version. */
export function useFlowEditorLayoutVersionSync(args: UseFlowEditorLayoutVersionArgs): void {
  const { layoutVersion, currentLayoutVersion, nodesInitialized, onReLayout, onLayoutVersionChange } = args;

  useEffect(() => {
    if (layoutVersion === currentLayoutVersion || !nodesInitialized) return;
    onReLayout();
    onLayoutVersionChange(currentLayoutVersion);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [layoutVersion, nodesInitialized]);
}
