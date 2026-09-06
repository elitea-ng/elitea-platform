import { fireEvent } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { ToolListError } from '.';

describe('ToolListError', () => {
  it('names the failure and offers a retry', () => {
    const onRetry = vi.fn();
    const { getByTestId, getByText, getByRole } = renderWithTheme(<ToolListError onRetry={onRetry} />);

    expect(getByTestId('tool-list-error')).toBeInTheDocument();
    expect(getByText('The tool list did not load. Try again.')).toBeInTheDocument();

    fireEvent.click(getByRole('button', { name: 'Retry' }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it('takes the test id of the picker it replaces, so one screen can hold more than one', () => {
    const { getByTestId, queryByTestId } = renderWithTheme(
      <ToolListError
        onRetry={vi.fn()}
        testId="loop-tool-list-error"
      />,
    );

    expect(getByTestId('loop-tool-list-error')).toBeInTheDocument();
    expect(queryByTestId('tool-list-error')).not.toBeInTheDocument();
  });

  it('takes a caller message for a read that is not a tool list', () => {
    const { getByText, queryByText } = renderWithTheme(
      <ToolListError
        onRetry={vi.fn()}
        message="The tool settings did not load. Try again."
      />,
    );

    expect(getByText('The tool settings did not load. Try again.')).toBeInTheDocument();
    expect(queryByText('The tool list did not load. Try again.')).not.toBeInTheDocument();
  });
});
