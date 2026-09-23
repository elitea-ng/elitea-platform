/**
 * `SendButton`: the platform Voice Features switches, and the accessible name
 * of the composer's primary action.
 *
 * ## Defect 1 — the voice flags were hardcoded
 *
 * `SendButton` held `const VOICE_FEATURES_ENABLED = true` and
 * `const VOICE_FEATURES_TEMPORARILY_DISABLED = false` as module constants.
 * `GET /elitea_core/platform_settings/…` already published the real
 * `voice_features_enabled` and `voice_features_temporarily_disabled` values.
 * The sibling `VoiceButton` already read them. An operator who switched voice
 * off in the admin panel still got the voice-wave control in the composer. A
 * click on it opened the ASR pipeline that the operator had just closed. With
 * `voice_features_temporarily_disabled = true` the control stayed clickable,
 * and it never showed the administrator tooltip.
 *
 * ## Defect 2 — the send control had no role and no name
 *
 * Both controls were `<Box component="span" onClick=…>` around an `aria-hidden`
 * MUI icon. They had no role, no `tabIndex`, no key handler and no accessible
 * name. A keyboard user could not reach the product's primary action. A screen
 * reader announced nothing where the send icon sits. The assertions below query
 * by ROLE and NAME, so the old markup cannot satisfy them.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { SendButton } from './SendButton';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const SETTINGS_URL = '*/elitea_core/platform_settings/prompt_lib';

function Harness({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return (
    <QueryClientProvider client={client}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        {children}
      </ThemeProvider>
    </QueryClientProvider>
  );
}

function serveFlags(enabled: boolean, temporarilyDisabled: boolean): void {
  server.use(
    http.get(SETTINGS_URL, () =>
      HttpResponse.json({
        voice_features_enabled: enabled,
        voice_features_temporarily_disabled: temporarilyDisabled,
      }),
    ),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('SendButton honours the admin Voice Features switches', () => {
  it('offers the voice-wave control on an empty composer while voice is enabled', async () => {
    serveFlags(true, false);
    const onEnterSpeakingMode = vi.fn();
    render(
      <Harness>
        <SendButton question="" onEnterSpeakingMode={onEnterSpeakingMode} />
      </Harness>,
    );

    const mic = await screen.findByRole('button', { name: 'Start speaking' });
    expect(mic).toBeEnabled();
    await userEvent.click(mic);
    expect(onEnterSpeakingMode).toHaveBeenCalledTimes(1);
  });

  it('falls back to the send arrow once an administrator switches voice off', async () => {
    serveFlags(false, false);
    const onEnterSpeakingMode = vi.fn();
    render(
      <Harness>
        <SendButton question="" onEnterSpeakingMode={onEnterSpeakingMode} />
      </Harness>,
    );

    // The mic is present first, because the flags default to enabled while
    // the query is in flight. It must give way to the send arrow when the
    // answer lands. An immediate assertion also passes against a component
    // that renders no mic at all. The case above rules that component out.
    await waitFor(() => {
      expect(screen.queryByRole('button', { name: 'Start speaking' })).not.toBeInTheDocument();
    });
    expect(screen.getByTestId('chat-send-button')).toBeInTheDocument();
    expect(onEnterSpeakingMode).not.toHaveBeenCalled();
  });

  it('hides the speaking-mode strip when voice is switched off mid-session', async () => {
    serveFlags(false, false);
    render(
      <Harness>
        <SendButton question="" isSpeakingMode />
      </Harness>,
    );

    await waitFor(() => {
      expect(screen.queryByText('Speaking...')).not.toBeInTheDocument();
    });
  });

  it('keeps the voice control visible, non-interactive and explained when temporarily disabled', async () => {
    serveFlags(true, true);
    const onEnterSpeakingMode = vi.fn();
    render(
      <Harness>
        <SendButton question="" onEnterSpeakingMode={onEnterSpeakingMode} />
      </Harness>,
    );

    // The tooltip text is not the whole fix. A control that says "Temporarily
    // disabled by admin" and still enters speaking mode on click lies to the
    // operator and to the user.
    const mic = await screen.findByRole('button', { name: 'Temporarily disabled by admin' });
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Temporarily disabled by admin' })).toBeDisabled();
    });
    // `fireEvent`, not `userEvent`: a disabled MUI button carries
    // `pointer-events: none`, which makes `userEvent` refuse the interaction
    // before React sees it. `fireEvent` dispatches the click anyway, so this
    // asserts the handler itself, not the cursor style.
    fireEvent.click(mic);
    expect(onEnterSpeakingMode).not.toHaveBeenCalled();
  });
});

describe('SendButton exposes the send action to keyboard and screen readers', () => {
  it('names the send control and reaches it with the keyboard', async () => {
    serveFlags(true, false);
    const onSend = vi.fn();
    render(
      <Harness>
        <SendButton question="hello" onSend={onSend} />
      </Harness>,
    );

    const send = await screen.findByRole('button', { name: 'Send' });
    expect(send).toBeEnabled();
    send.focus();
    expect(send).toHaveFocus();
    await userEvent.keyboard('{Enter}');
    expect(onSend).toHaveBeenCalledTimes(1);
  });

  it('disables the send control instead of leaving a silent unclickable span', async () => {
    serveFlags(true, false);
    const onSend = vi.fn();
    render(
      <Harness>
        <SendButton question="hello" disabledSend onSend={onSend} />
      </Harness>,
    );

    const send = await screen.findByRole('button', { name: 'Send' });
    expect(send).toBeDisabled();
    fireEvent.click(send);
    expect(onSend).not.toHaveBeenCalled();
  });
});

describe('issue 932: an active voice recording blocks the speaking-mode wave icon only', () => {
  it('disables the wave icon while a recording is capturing', async () => {
    serveFlags(true, false);
    render(<Harness><SendButton question="" isRecording /></Harness>);
    await waitFor(() => expect(screen.getByTestId('chat-speaking-mode-button')).toBeDisabled());
  });

  it('leaves the wave icon live when nothing is recording', async () => {
    serveFlags(true, false);
    render(<Harness><SendButton question="" /></Harness>);
    await waitFor(() => expect(screen.getByTestId('chat-speaking-mode-button')).toBeEnabled());
  });

  it('does NOT disable Send for text already in the composer', async () => {
    serveFlags(true, false);
    render(<Harness><SendButton question="dictated text" isRecording /></Harness>);
    await waitFor(() => expect(screen.getByTestId('chat-send-button')).toBeEnabled());
  });
});
