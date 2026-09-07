/**
 * One source's ingestion status, as a chip.
 *
 * The mapping from a provider word to a colour and a sentence is the part of
 * this screen that is easy to get silently wrong, and it is rendered in two
 * places. Two readings must never be folded together: a source that was ADDED
 * AND NEVER INGESTED is waiting, and a word this table has no reading for is
 * information from a newer engine. Printing the second one as "Not ingested"
 * tells the user the opposite of what the provider said.
 */
import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithProviders } from '../__tests__/testUtils';
import { SourceStatusChip } from './SourceStatusChip';

function chip(): HTMLElement {
  return screen.getByTestId('inventory-source-status');
}

describe('SourceStatusChip', () => {
  it('reads each known status into its own copy and kind', () => {
    for (const [status, kind, label] of [
      ['pending', 'pending', 'Pending'],
      ['in_progress', 'ingesting', 'Ingesting…'],
      ['completed', 'done', 'Done'],
      ['error', 'error', 'Error'],
    ] as const) {
      const { unmount } = renderWithProviders(<SourceStatusChip status={status} />);
      expect(chip()).toHaveAttribute('data-status', kind);
      expect(chip()).toHaveTextContent(label);
      unmount();
    }
  });

  it('shows a source that has never been ingested as waiting', () => {
    renderWithProviders(<SourceStatusChip status="" />);
    expect(chip()).toHaveAttribute('data-status', 'waiting');
    expect(chip()).toHaveTextContent('Not ingested');
  });

  it('prints an unknown status AS IT STANDS, with no kind claimed for it', () => {
    renderWithProviders(<SourceStatusChip status="quarantined" />);
    expect(chip()).toHaveTextContent('quarantined');
    expect(chip()).not.toHaveAttribute('data-status');
  });
});
