import { DeprecatedConstants, FlowEditorConstants, RuntimeContractConstants } from '../../lib/flow-editor/constants';
import type { PipelineNodeType } from '../../lib/flow-editor/constants/flowEditor.constants';

// Sizes in px, matching the NodeTypeSelector styles (1rem = 16px)
const CARD_WIDTH = 258;
const GRID_GAP = 8;
const PAPER_PADDING_X = 32;
const PAPER_BORDER = 2;
const ANCHOR_GAP = 8;
export const BOUNDARY_MARGIN = 24;
const MIN_HEIGHT = 200;

export const TWO_COLUMNS_WIDTH = CARD_WIDTH * 2 + GRID_GAP + PAPER_PADDING_X + PAPER_BORDER;

export interface SelectorRect {
  readonly top: number;
  readonly left: number;
  readonly width: number;
  readonly height: number;
  readonly bottom: number;
  readonly right: number;
}

export interface NodeTypeSelectorLayoutInput {
  readonly boundaryRect?: SelectorRect | null | undefined;
  readonly anchorRect?: SelectorRect | null | undefined;
  readonly isCentered?: boolean | undefined;
}

export interface NodeTypeSelectorLayout {
  readonly columns: 1 | 2;
  readonly maxWidth: number | null;
  readonly maxHeight: number | null;
}

/**
 * Fits the node type selector into the canvas it opens over:
 * two columns when the canvas is wide enough, otherwise one, and a max height so it scrolls
 * inside the canvas instead of overflowing it.
 */
export function getNodeTypeSelectorLayout({
  boundaryRect,
  anchorRect,
  isCentered = false,
}: NodeTypeSelectorLayoutInput): NodeTypeSelectorLayout {
  if (!boundaryRect) {
    return { columns: 2, maxWidth: null, maxHeight: null };
  }

  const maxWidth = Math.max(boundaryRect.width - BOUNDARY_MARGIN * 2, CARD_WIDTH);
  const availableHeight =
    isCentered || !anchorRect
      ? boundaryRect.height - BOUNDARY_MARGIN * 2
      : boundaryRect.bottom - anchorRect.bottom - ANCHOR_GAP - BOUNDARY_MARGIN;

  return {
    columns: maxWidth >= TWO_COLUMNS_WIDTH ? 2 : 1,
    maxWidth,
    maxHeight: Math.max(availableHeight, MIN_HEIGHT),
  };
}

/**
 * All compiler-admitted, non-deprecated node types in alphabetical display-name order
 * (the 14 replatform-admitted types).
 */
export function getAddableNodeTypes(): readonly PipelineNodeType[] {
  const types = FlowEditorConstants.PipelineNodeTypes as Readonly<Record<string, PipelineNodeType>>;
  const displayNames = FlowEditorConstants.PipelineNodeDisplayNames;

  return Object.keys(types)
    .filter((typeName) => !DeprecatedConstants.DeprecatedOrInvisibleNode.includes(typeName))
    .map((typeName) => types[typeName])
    .filter((value): value is PipelineNodeType => value !== undefined)
    .filter((value) => RuntimeContractConstants.isCompilerAdmittedNodeType(value))
    .sort((a, b) => displayNames[a].toLowerCase().localeCompare(displayNames[b].toLowerCase()));
}

/** Normalizes a node type id (e.g. `state_modifier`, `split_out`) to kebab-case for `data-testid`. */
export function toNodeTypeTestIdSegment(type: string): string {
  return type.toLowerCase().replace(/_/g, '-');
}
