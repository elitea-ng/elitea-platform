/**
 * `@xyflow/react`-typed node/edge aliases for A2d's owned canvas-interaction
 * helpers/hooks. Not a baseline port — the baseline is plain JS and never
 * types its React Flow nodes/edges at all.
 *
 * Deliberately separate from `../model/pipelineFlow.types.ts` (unit A2c):
 * that module's `FlowGraphNode`/`FlowGraphEdge` are intentionally
 * *structural*, decoupled from `@xyflow/react`'s generics, because most of
 * A2c's helpers never touch the library's runtime (`addEdge`,
 * `useReactFlow`, `useKeyPress`, `reconnectEdge`). A2d's owned files DO
 * call directly into that runtime (`useConnectNodes`, `useIncompleteEdge`,
 * `useDeleteItems`, `useCtrlASelectAll`, `layout.helpers.ts`'s dagre pass),
 * so the richer `Node<T>`/`Edge<T>` generics (`measured`, `selected`,
 * `position`) are the right tool here — and are still structurally
 * compatible with `FlowGraphNode`/`FlowGraphConnection` wherever this file's
 * types are passed into A2c's helpers (a wider struct is assignable to a
 * narrower one).
 */
import type { Edge, Node } from '@xyflow/react';

import type { PipelineYamlEditOptions } from '../pipelineYamlDocument.helpers';

import type { YamlConditionSpec, YamlDecisionSpec, YamlPipelineDocument } from './helpers/pipelineFlow.types';

/** `node.data` — fields read/written by flowNodeUpdate/layout/deletionOperations helpers. */
export interface FlowNodeData {
  readonly label?: string;
  readonly condition?: YamlConditionSpec;
  readonly decision?: YamlDecisionSpec;
  readonly isPerforming?: boolean;
  readonly status?: string;
  readonly timeline?: readonly unknown[];
  readonly [key: string]: unknown;
}

export type FlowNode = Node<FlowNodeData>;

/** `edge.data` — `{ label: 'Interrupt' }` on interrupt-crossing edges only. */
export interface FlowEdgeData {
  readonly label?: string;
  readonly [key: string]: unknown;
}

export type FlowEdge = Edge<FlowEdgeData>;

export type SetFlowNodes = (updater: FlowNode[] | ((prev: FlowNode[]) => FlowNode[])) => void;
export type SetFlowEdges = (updater: FlowEdge[] | ((prev: FlowEdge[]) => FlowEdge[])) => void;
export type SetYamlJsonObject = (next: YamlPipelineDocument, options?: PipelineYamlEditOptions) => void;

export interface YamlPipelineDocumentRef {
  current: YamlPipelineDocument;
}
