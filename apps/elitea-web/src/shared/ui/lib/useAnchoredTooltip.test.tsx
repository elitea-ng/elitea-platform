import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import Tooltip from '@mui/material/Tooltip';
import { describe, expect, it } from 'vitest';

import { useAnchoredTooltip } from './useAnchoredTooltip';

function Harness({ enterDelay = 0 }: { readonly enterDelay?: number }) {
  const tip = useAnchoredTooltip<HTMLButtonElement>();
  return (
    <>
      <Tooltip title="Expand sidebar" enterDelay={enterDelay} {...tip.tooltipProps}>
        <button ref={tip.anchorRef} type="button" onClick={tip.close}>
          toggle
        </button>
      </Tooltip>
      <div data-testid="elsewhere">elsewhere</div>
    </>
  );
}

describe('useAnchoredTooltip', () => {
  it('closes when the pointer moves anywhere but the anchor, even without a mouseleave', async () => {
    render(<Harness />);
    act(() => {
      tipOpener();
    });
    expect(screen.getByRole('tooltip')).toBeInTheDocument();

    // Over the anchor: stays.
    fireEvent.pointerMove(screen.getByRole('button', { name: 'Expand sidebar' }));
    expect(screen.getByRole('tooltip')).toBeInTheDocument();

    // Elsewhere, with no mouseleave on the anchor (it moved away): closes.
    fireEvent.pointerMove(screen.getByTestId('elsewhere'));
    await waitFor(() => expect(screen.queryByRole('tooltip')).toBeNull());
  });

  it('closes on the click that moves the anchor', async () => {
    render(<Harness />);
    act(() => {
      tipOpener();
    });
    expect(screen.getByRole('tooltip')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Expand sidebar' }));
    await waitFor(() => expect(screen.queryByRole('tooltip')).toBeNull());
  });

  it('stays shut when the enter delay that the pre-click hover started runs out after the click', async () => {
    render(<Harness enterDelay={30} />);
    fireEvent.mouseOver(screen.getByRole('button', { name: 'Expand sidebar' }));
    fireEvent.click(screen.getByRole('button', { name: 'Expand sidebar' }));
    await new Promise((resolve) => setTimeout(resolve, 80));
    expect(screen.queryByRole('tooltip')).toBeNull();

    // Once the pointer has been elsewhere, a fresh hover opens it again.
    fireEvent.pointerMove(screen.getByTestId('elsewhere'));
    fireEvent.mouseLeave(screen.getByRole('button', { name: 'Expand sidebar' }));
    fireEvent.mouseOver(screen.getByRole('button', { name: 'Expand sidebar' }));
    expect(await screen.findByRole('tooltip')).toBeInTheDocument();
  });
});

/** MUI opens on `mouseover` of the child. */
function tipOpener(): void {
  fireEvent.mouseOver(screen.getByRole('button', { name: 'Expand sidebar' }));
}
