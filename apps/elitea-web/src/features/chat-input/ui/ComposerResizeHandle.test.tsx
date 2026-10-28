import { fireEvent, screen } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';

import { clearNamespace } from '@/shared/lib/storage';
import { renderWithTheme as render } from '@/shared/ui/lib/testTheme';

import { useComposerResize } from './ComposerResizeHandle';

function Harness({ native }: { readonly native: boolean }): React.JSX.Element {
  const { handle, minRows } = useComposerResize(native);
  return (
    <>
      {handle}
      <output data-testid="lines">{String(minRows)}</output>
    </>
  );
}

beforeEach(() => clearNamespace());

describe('the desktop composer resize handle', () => {
  it('is the desktop only: the web keeps its expand button', () => {
    render(<Harness native={false} />);
    expect(screen.queryByTestId('composer-resize-handle')).toBeNull();
    expect(screen.getByTestId('lines')).toHaveTextContent('undefined');
  });

  it('moves a line per arrow key, goes to the ends, resets on a double click, and is remembered', () => {
    const first = render(<Harness native />);
    const handle = screen.getByRole('separator', { name: 'Resize the message box' });
    expect(handle).toHaveAttribute('aria-valuenow', '2');
    fireEvent.keyDown(handle, { key: 'ArrowUp' });
    fireEvent.keyDown(handle, { key: 'ArrowUp' });
    expect(screen.getByTestId('lines')).toHaveTextContent('4');
    fireEvent.keyDown(handle, { key: 'End' });
    expect(handle).toHaveAttribute('aria-valuenow', '24');
    fireEvent.keyDown(handle, { key: 'Home' });
    expect(handle).toHaveAttribute('aria-valuenow', '1');
    fireEvent.keyDown(handle, { key: 'ArrowUp' });
    fireEvent.keyDown(handle, { key: 'ArrowUp' });
    fireEvent.keyDown(handle, { key: 'ArrowUp' });
    first.unmount();
    render(<Harness native />);
    expect(screen.getByTestId('lines')).toHaveTextContent('4');
    fireEvent.doubleClick(screen.getByRole('separator'));
    expect(screen.getByTestId('lines')).toHaveTextContent('2');
  });

  it('grows as it is dragged up, a line per 20px', () => {
    render(<Harness native />);
    const handle = screen.getByRole('separator');
    handle.setPointerCapture = () => undefined;
    fireEvent.pointerDown(handle, { clientY: 500, pointerId: 1 });
    fireEvent.pointerMove(handle, { clientY: 440, pointerId: 1 });
    fireEvent.pointerUp(handle, { pointerId: 1 });
    expect(screen.getByTestId('lines')).toHaveTextContent('5');
  });
});
