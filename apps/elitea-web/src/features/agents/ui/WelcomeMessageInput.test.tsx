import type { ReactNode } from 'react';
import { useState } from 'react';

import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen } from '@testing-library/react';

import { renderWithProviders } from '../__tests__/testUtils';

import { WelcomeMessageInput } from './WelcomeMessageInput';

describe('WelcomeMessageInput', () => {
  it('renders the current welcome message', () => {
    renderWithProviders(
      <WelcomeMessageInput
        welcomeMessage="Hello there"
        onWelcomeMessageChange={vi.fn()}
        versionId={1}
      />,
    );
    expect(screen.getByDisplayValue('Hello there')).toBeInTheDocument();
  });

  it('calls onWelcomeMessageChange as the user types', () => {
    const onWelcomeMessageChange = vi.fn();
    renderWithProviders(
      <WelcomeMessageInput
        welcomeMessage=""
        onWelcomeMessageChange={onWelcomeMessageChange}
        versionId={1}
      />,
    );
    fireEvent.change(screen.getByTestId('agent-welcome-message-input'), { target: { value: 'Hi!' } });
    expect(onWelcomeMessageChange).toHaveBeenCalledWith('Hi!');
  });

  it('re-syncs local state when welcomeMessage changes externally without a versionId change', () => {
    // Baseline (`components/WelcomeMessage.jsx`) resyncs on ANY external
    // change to the value, not just a version switch — e.g. a future
    // discard/reset action that resets `welcomeMessage` while `versionId`
    // stays the same.
    function Host(): ReactNode {
      const [message, setMessage] = useState('First message');
      return (
        <div>
          <button onClick={() => setMessage('Reset message')}>reset</button>
          <WelcomeMessageInput
            welcomeMessage={message}
            onWelcomeMessageChange={vi.fn()}
            versionId={1}
          />
        </div>
      );
    }

    renderWithProviders(<Host />);
    expect(screen.getByDisplayValue('First message')).toBeInTheDocument();

    fireEvent.click(screen.getByText('reset'));
    expect(screen.getByDisplayValue('Reset message')).toBeInTheDocument();
  });

  it('re-syncs local state when versionId changes (version switch)', () => {
    // A tiny stateful host, rendered once via `renderWithProviders` (so the
    // MUI theme/query-client wrapper survives the update) — clicking
    // "switch version" mimics the caller supplying a new `versionId` + `welcomeMessage`.
    function Host(): ReactNode {
      const [version, setVersion] = useState<{ id: number; message: string }>({ id: 1, message: 'First version' });
      return (
        <div>
          <button onClick={() => setVersion({ id: 2, message: 'Second version' })}>switch version</button>
          <WelcomeMessageInput
            welcomeMessage={version.message}
            onWelcomeMessageChange={vi.fn()}
            versionId={version.id}
          />
        </div>
      );
    }

    renderWithProviders(<Host />);
    expect(screen.getByDisplayValue('First version')).toBeInTheDocument();

    fireEvent.click(screen.getByText('switch version'));
    expect(screen.getByDisplayValue('Second version')).toBeInTheDocument();
  });

  // #848 — the counter used to be conditionally MOUNTED (`showCounter &&
  // <Typography>`), so blurring the field removed it from the tree and
  // shrank whatever sits below it (`+ Starter` in the real agent editor,
  // rendered right after this component). jsdom does not compute real
  // layout, so an `offsetTop` assertion on a sibling would prove nothing —
  // it always reads 0. What DOES prove the fix: the exact same DOM node
  // survives focus -> blur (an unmount+remount would hand back a different
  // node from a fresh query), and its reserved box never leaves the tree.
  it('keeps the counter mounted (same node) across focus and blur, only toggling visibility', () => {
    renderWithProviders(
      <div>
        <WelcomeMessageInput
          welcomeMessage=""
          onWelcomeMessageChange={vi.fn()}
          versionId={1}
        />
        <button type="button">+ Starter</button>
      </div>,
    );

    const input = screen.getByTestId('agent-welcome-message-input');

    // Present before any typing/focus at all — the line is reserved from the start.
    const counterAtRest = screen.getByTestId('agent-welcome-message-counter');
    expect(getComputedStyle(counterAtRest).visibility).toBe('hidden');

    fireEvent.change(input, { target: { value: 'Hi' } });
    fireEvent.focus(input);
    const counterFocused = screen.getByTestId('agent-welcome-message-counter');
    expect(counterFocused).toBe(counterAtRest);
    expect(getComputedStyle(counterFocused).visibility).toBe('visible');

    fireEvent.blur(input);
    const counterBlurred = screen.getByTestId('agent-welcome-message-counter');
    expect(counterBlurred).toBe(counterAtRest);
    expect(getComputedStyle(counterBlurred).visibility).toBe('hidden');

    // The control below is still reachable by a direct query the whole time
    // (this is the control a real mousedown-driven layout shift would have
    // moved out from under the pointer).
    expect(screen.getByRole('button', { name: '+ Starter' })).toBeInTheDocument();
  });
});
