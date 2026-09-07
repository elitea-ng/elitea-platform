/**
 * The ask panel.
 *
 * THREE OF ITS FOUR GUARANTEES ARE ABOUT A WAIT THE USER CANNOT SEE INTO, and
 * each fails as a screen that looks fine:
 *
 *  - while a question runs the composer must be UNUSABLE and the Stop control
 *    present. A panel that accepted a second question would interleave two
 *    runs' progress in one transcript.
 *  - the progress lines are the only evidence the agent is working; a panel
 *    that dropped them shows a spinner for as long as the agent takes.
 *  - a CITATION must be clickable. An answer the reader cannot check against
 *    the graph it came from is an answer they have to trust, and the click is
 *    the whole difference.
 *
 * The fourth is the Clear control, which exists because the controller's
 * `clear()` had no caller — the dead-affordance shape this repository keeps
 * finding. It must appear only once there is something to clear.
 */
import { describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithProviders } from '../__tests__/testUtils';
import { InventoryAskPanel } from './InventoryAskPanel';

const TURN = {
  question: 'What places orders?',
  answer: 'CheckoutService places orders.',
  entities: ['code:checkout-service'],
};

function props(overrides: Partial<Parameters<typeof InventoryAskPanel>[0]> = {}) {
  return {
    turns: [],
    pendingQuestion: null,
    steps: [],
    error: null,
    onAsk: vi.fn(),
    onStop: vi.fn(),
    onClear: vi.fn(),
    onOpenEntity: vi.fn(),
    ...overrides,
  } as Parameters<typeof InventoryAskPanel>[0];
}

describe('InventoryAskPanel', () => {
  it('invites a question, and offers nothing to clear yet', () => {
    renderWithProviders(<InventoryAskPanel {...props()} />);
    expect(screen.getByTestId('inventory-ask-empty')).toBeVisible();
    // The Clear control would clear nothing. An affordance that does nothing
    // is worse than none: it reads as a broken button.
    expect(screen.queryByTestId('inventory-ask-clear')).toBeNull();
  });

  it('sends a typed question once and empties the composer', async () => {
    const onAsk = vi.fn();
    renderWithProviders(<InventoryAskPanel {...props({ onAsk })} />);

    const input = screen.getByTestId('inventory-ask-input');
    await userEvent.type(input, 'What places orders?');
    await userEvent.click(screen.getByTestId('inventory-ask-send'));

    expect(onAsk).toHaveBeenCalledExactlyOnceWith('What places orders?');
    expect(input).toHaveValue('');
  });

  it('refuses an empty question rather than asking one', async () => {
    const onAsk = vi.fn();
    renderWithProviders(<InventoryAskPanel {...props({ onAsk })} />);
    await userEvent.type(screen.getByTestId('inventory-ask-input'), '   ');
    await userEvent.click(screen.getByTestId('inventory-ask-send'));
    expect(onAsk).not.toHaveBeenCalled();
  });

  it('shows what the agent is doing, and offers Stop instead of Ask', async () => {
    const onStop = vi.fn();
    renderWithProviders(
      <InventoryAskPanel
        {...props({
          pendingQuestion: 'What places orders?',
          steps: ['Planning the investigation', 'Reading the graph'],
          onStop,
        })}
      />,
    );

    const steps = screen.getByTestId('inventory-ask-steps');
    expect(steps).toHaveTextContent('Planning the investigation');
    expect(steps).toHaveTextContent('Reading the graph');
    expect(screen.getByTestId('inventory-ask-input')).toBeDisabled();
    expect(screen.queryByTestId('inventory-ask-send')).toBeNull();

    await userEvent.click(screen.getByTestId('inventory-ask-stop'));
    expect(onStop).toHaveBeenCalledTimes(1);
  });

  it('says the graph is being read when the agent has not spoken yet', () => {
    renderWithProviders(<InventoryAskPanel {...props({ pendingQuestion: 'anything', steps: [] })} />);
    expect(screen.getByTestId('inventory-ask-steps')).toHaveTextContent(/Reading the graph/);
  });

  it('renders the answer and opens the entity a citation names', async () => {
    const onOpenEntity = vi.fn();
    renderWithProviders(<InventoryAskPanel {...props({ turns: [TURN], onOpenEntity })} />);

    expect(screen.getByTestId('inventory-ask-answer')).toHaveTextContent('CheckoutService places orders.');
    await userEvent.click(screen.getByTestId('inventory-ask-citation'));
    expect(onOpenEntity).toHaveBeenCalledExactlyOnceWith('code:checkout-service');
  });

  it('omits the citation row for an answer that named nothing', () => {
    renderWithProviders(
      <InventoryAskPanel {...props({ turns: [{ ...TURN, entities: [] }] })} />,
    );
    expect(screen.queryByTestId('inventory-ask-citations')).toBeNull();
  });

  it('offers Clear once there is a transcript, and reports the press', async () => {
    const onClear = vi.fn();
    renderWithProviders(<InventoryAskPanel {...props({ turns: [TURN], onClear })} />);
    await userEvent.click(screen.getByTestId('inventory-ask-clear'));
    expect(onClear).toHaveBeenCalledTimes(1);
  });

  it('reports a refusal instead of an empty answer', () => {
    renderWithProviders(<InventoryAskPanel {...props({ error: 'No graph is loaded.' })} />);
    expect(screen.getByText('No graph is loaded.')).toBeVisible();
  });
});
