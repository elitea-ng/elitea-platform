import { ReactFlowProvider } from '@xyflow/react';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CanvasControls, CanvasMiniMap } from './index';

describe('CanvasControls & CanvasMiniMap', () => {
  it('renders zoom/fit controls, toggles lock and minimap, and selects TB/LR arrange direction', async () => {
    const user = userEvent.setup();
    const onToggleLock = vi.fn();
    const onArrange = vi.fn();
    const onToggleMinimap = vi.fn();

    renderWithTheme(
      <ReactFlowProvider>
        <CanvasControls
          isLocked={false}
          onToggleLock={onToggleLock}
          orientation="TB"
          onArrange={onArrange}
          isMinimapVisible={false}
          onToggleMinimap={onToggleMinimap}
        />
        <CanvasMiniMap rightOffsetPx={400} />
      </ReactFlowProvider>,
    );

    await user.click(screen.getByTestId('pipeline-canvas-zoom-in'));
    await user.click(screen.getByTestId('pipeline-canvas-zoom-out'));
    await user.click(screen.getByTestId('pipeline-canvas-fit-view'));
    await user.click(screen.getByTestId('pipeline-canvas-lock'));
    await user.click(screen.getByTestId('pipeline-canvas-minimap-toggle'));

    expect(onToggleLock).toHaveBeenCalledTimes(1);
    expect(onToggleMinimap).toHaveBeenCalledTimes(1);

    await user.click(screen.getByTestId('pipeline-canvas-arrange'));
    expect(screen.getByTestId('pipeline-canvas-arrange-tb')).toHaveAttribute('data-current', 'true');
    await user.click(screen.getByTestId('pipeline-canvas-arrange-lr'));
    expect(onArrange).toHaveBeenCalledWith('LR');
    expect(screen.getByTestId('rf__minimap')).toBeInTheDocument();
  });

  it('disables auto-arrange when canArrange is false', () => {
    renderWithTheme(
      <ReactFlowProvider>
        <CanvasControls
          isLocked
          onToggleLock={vi.fn()}
          orientation="LR"
          onArrange={vi.fn()}
          canArrange={false}
          isMinimapVisible
          onToggleMinimap={vi.fn()}
        />
      </ReactFlowProvider>,
    );

    expect(screen.getByTestId('pipeline-canvas-arrange')).toBeDisabled();
    expect(screen.getByTestId('pipeline-canvas-lock')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('pipeline-canvas-minimap-toggle')).toHaveAttribute('aria-pressed', 'true');
  });
});
