/**
 * Full auto's switch (issue 3079): offered only while the host says the
 * organisation allows it, confirmed every time it is turned on, never kept
 * (a new mount starts without it), and shown by the red banner whose button
 * turns it off.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { FullAutoControls } from './FullAutoControls';
import { useFullAutoMode } from './useFullAutoMode';

// The host's `full_auto_status` answer (`fullAutoIpc`), through the webview's invoke.
const invoke = vi.fn();
function host(allowed: boolean): void {
  invoke.mockImplementation((command: string) => (command === 'full_auto_status' ? Promise.resolve({ allowed }) : Promise.reject(new Error(command))));
  vi.stubGlobal('__TAURI_INTERNALS__', { invoke });
}

function Probe({ local, turnedOn }: { local: boolean; turnedOn: () => void }): React.JSX.Element {
  const [planMode, setPlanMode] = useState(false);
  const fullAuto = useFullAutoMode(local, planMode);
  return (
    <>
      <button type="button" onClick={() => setPlanMode((value) => !value)}>
        plan
      </button>
      <span data-testid="state">{`${String(fullAuto.allowed)}/${String(fullAuto.on)}`}</span>
      <button type="button" onClick={() => fullAuto.request(true, turnedOn)}>
        on
      </button>
      <FullAutoControls fullAuto={fullAuto} active={fullAuto.on} />
    </>
  );
}

function mount(local = true, turnedOn = vi.fn()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <Probe local={local} turnedOn={turnedOn} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  invoke.mockReset();
});

describe('Full auto', () => {
  it('asks every time it is turned on, shows the banner while on, and the banner turns it off', async () => {
    host(true);
    const turnedOn = vi.fn();
    const user = userEvent.setup();
    mount(true, turnedOn);
    await waitFor(() => expect(screen.getByTestId('state')).toHaveTextContent('true/false'));

    // A cancelled confirmation keeps it off.
    await user.click(screen.getByRole('button', { name: 'on' }));
    expect(await screen.findByRole('dialog', { name: 'Run this thread in Full auto?' })).toHaveTextContent('outside the sandbox');
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByTestId('state')).toHaveTextContent('true/false');
    expect(turnedOn).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: 'on' }));
    await user.click(await screen.findByTestId('workspace-full-auto-confirm'));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByTestId('state')).toHaveTextContent('true/true');
    expect(turnedOn).toHaveBeenCalledOnce();
    expect(screen.getByTestId('workspace-full-auto-banner')).toHaveTextContent('Full auto: the agent answers every approval');

    await user.click(screen.getByRole('button', { name: 'Turn off' }));
    expect(screen.getByTestId('state')).toHaveTextContent('true/false');
    expect(screen.queryByTestId('workspace-full-auto-banner')).toBeNull();

    // The second turn-on asks again.
    await user.click(screen.getByRole('button', { name: 'on' }));
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
  });

  it('is not offered when the organisation forbids it, nor for the cloud copy', async () => {
    host(false);
    const user = userEvent.setup();
    const view = mount();
    await waitFor(() => expect(invoke).toHaveBeenCalled());
    await user.click(screen.getByRole('button', { name: 'on' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByTestId('state')).toHaveTextContent('false/false');
    view.unmount();

    host(true);
    mount(false);
    expect(invoke).toHaveBeenCalledOnce();
    expect(screen.getByTestId('state')).toHaveTextContent('false/false');
  });

  it('starts off in a new mount: it is never kept', async () => {
    host(true);
    const user = userEvent.setup();
    const view = mount();
    await waitFor(() => expect(screen.getByTestId('state')).toHaveTextContent('true/false'));
    await user.click(screen.getByRole('button', { name: 'on' }));
    await user.click(await screen.findByTestId('workspace-full-auto-confirm'));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByTestId('state')).toHaveTextContent('true/true');
    view.unmount();

    mount();
    await waitFor(() => expect(screen.getByTestId('state')).toHaveTextContent('true/false'));
  });

  it('ends with plan mode: leaving plan mode (a `/plan` round trip) does not turn it back on unasked', async () => {
    host(true);
    const user = userEvent.setup();
    mount();
    await waitFor(() => expect(screen.getByTestId('state')).toHaveTextContent('true/false'));
    await user.click(screen.getByRole('button', { name: 'on' }));
    await user.click(await screen.findByTestId('workspace-full-auto-confirm'));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByTestId('state')).toHaveTextContent('true/true');
    await user.click(screen.getByRole('button', { name: 'plan' }));
    expect(screen.getByTestId('state')).toHaveTextContent('true/false');
    await user.click(screen.getByRole('button', { name: 'plan' }));
    expect(screen.getByTestId('state')).toHaveTextContent('true/false');
    expect(screen.queryByTestId('workspace-full-auto-banner')).toBeNull();
  });
});
