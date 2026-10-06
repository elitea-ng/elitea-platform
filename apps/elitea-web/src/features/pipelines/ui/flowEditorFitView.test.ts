import { getViewportForBounds } from '@xyflow/react';
import { afterEach, expect, it, vi } from 'vitest';

import { flowEditorFitViewOptions } from './flowEditorFitView';

const graphBounds = { x: 60, y: 200, width: 473, height: 1467 };

function canvas(width: number, controlWidth = 28) {
  const element = document.createElement('div');
  const controls = document.createElement('div');
  controls.className = 'react-flow__controls';
  element.append(controls);
  vi.spyOn(element, 'getBoundingClientRect').mockReturnValue(new DOMRect(616, 133, width, 573));
  vi.spyOn(controls, 'getBoundingClientRect').mockReturnValue(new DOMRect(628, 531, controlWidth, 170));
  return element;
}

afterEach(() => vi.restoreAllMocks());

it('fits restored graph bounds to the right of the fixed controls in a narrow shell canvas', () => {
  const width = 161;
  const options = flowEditorFitViewOptions(canvas(width));
  const ordinary = getViewportForBounds(graphBounds, width, 573, 0.1, 2, 0.1);
  const fitted = getViewportForBounds(graphBounds, width, 573, 0.1, 2, options?.padding ?? 0.1);
  const controlRight = 40;
  expect(ordinary.x + graphBounds.x * ordinary.zoom).toBeLessThan(controlRight);
  expect(fitted.x + graphBounds.x * fitted.zoom).toBeGreaterThan(controlRight);
  expect(fitted.x + (graphBounds.x + graphBounds.width) * fitted.zoom).toBeLessThan(width);
  expect(fitted.y + graphBounds.y * fitted.zoom).toBeGreaterThan(0);
  expect(fitted.y + (graphBounds.y + graphBounds.height) * fitted.zoom).toBeLessThan(573);
});

it('preserves the default fit when its existing padding already clears the controls', () => {
  const width = 1200;
  const options = flowEditorFitViewOptions(canvas(width));
  expect(getViewportForBounds(graphBounds, width, 573, 0.1, 2, options?.padding ?? 0.1))
    .toEqual(getViewportForBounds(graphBounds, width, 573, 0.1, 2, 0.1));
});

it('measures a wider control column instead of assuming a fixed button width', () => {
  const width = 240;
  const options = flowEditorFitViewOptions(canvas(width, 60));
  const fitted = getViewportForBounds(graphBounds, width, 573, 0.1, 2, options?.padding ?? 0.1);
  expect(fitted.x + graphBounds.x * fitted.zoom).toBeGreaterThan(72);
});

it('keeps the default fit when the canvas or controls are not measured', () => {
  expect(flowEditorFitViewOptions(null)).toBeUndefined();
  expect(flowEditorFitViewOptions(document.createElement('div'))).toBeUndefined();
  expect(flowEditorFitViewOptions(canvas(0))).toBeUndefined();
  expect(flowEditorFitViewOptions(canvas(161, 0))).toBeUndefined();
});
