import type { FitViewOptions } from '@xyflow/react';

const DEFAULT_PADDING = 0.1;
const CONTROL_GAP = 12;

export function flowEditorFitViewOptions(canvas: HTMLElement | null): FitViewOptions | undefined {
  const controls = canvas?.querySelector<HTMLElement>('.react-flow__controls');
  if (!canvas || !controls) return undefined;
  const canvasBounds = canvas.getBoundingClientRect();
  const controlsBounds = controls.getBoundingClientRect();
  if (canvasBounds.width <= 0 || controlsBounds.width <= 0) return undefined;
  const defaultLeft = Math.floor((canvasBounds.width - canvasBounds.width / (1 + DEFAULT_PADDING)) / 2);
  const left = Math.max(defaultLeft, controlsBounds.right - canvasBounds.left + CONTROL_GAP);
  return { padding: { x: DEFAULT_PADDING, y: DEFAULT_PADDING, left: `${left}px` } };
}
