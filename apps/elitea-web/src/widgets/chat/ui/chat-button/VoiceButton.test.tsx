/**
 * `VoiceButton` and the platform's Voice Features switches — unit A14, #200.
 *
 * This file exists for one reason: to prove that the admin switch reaches THIS
 * component. `VoiceButton` is the mounted voice control — `/chat` → `ChatBox` →
 * `buildChatBoxInputSlots()` → `<VoiceButton>` — and it used to hardcode both
 * flags as module constants (`VOICE_FEATURES_ENABLED = true`,
 * `VOICE_FEATURES_TEMPORARILY_DISABLED = false`), so the admin Features page had
 * offered switches named after this button that could not change it.
 *
 * That is the defect unit A14 exists to remove, and it is invisible to a test of
 * the admin page: the page saved, the server stored, the row was there — and the
 * button ignored it. The assertions below are on the RENDERED button, per flag
 * state.
 *
 * ## Why a fake global rather than a module mock
 *
 * `VoiceButton` returns null when no speech backend `isSupported`, which under
 * jsdom is always — so without a speech API the "hidden when off" case would
 * pass vacuously against a component that never rendered anything. The fake
 * `window.SpeechRecognition` from
 * `features/chat-input/lib/hooks/useSpeechRecognition.test.ts` is installed
 * instead of stubbing the hook: R-M1 allows substituting the network boundary
 * and the environment, not application modules, and a browser API absent from
 * jsdom is environment. The "renders while enabled" case is what keeps the fake
 * honest.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { HttpResponse, http } from 'msw';
import type { ReactNode } from 'react';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { VoiceButton, resolveDictationInsertPoint, withDictationSeparator } from './VoiceButton';
import type { VoiceButtonInputHandle } from './VoiceButton';

/** The minimum of the Web Speech API that `useSpeechRecognition` probes for. */
class FakeSpeechRecognition {
  continuous = false;
  interimResults = false;
  lang = '';
  onresult: ((event: unknown) => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;
  onend: (() => void) | null = null;
  start = vi.fn();
  stop = vi.fn();
  abort = vi.fn();
}

type WindowWithSpeechRecognition = typeof window & {
  SpeechRecognition?: typeof FakeSpeechRecognition;
  webkitSpeechRecognition?: typeof FakeSpeechRecognition;
};

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

function renderButton(): void {
  render(
    <Harness>
      <VoiceButton />
    </Harness>,
  );
}

beforeEach(() => {
  (window as WindowWithSpeechRecognition).SpeechRecognition = FakeSpeechRecognition;
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  delete (window as WindowWithSpeechRecognition).SpeechRecognition;
  delete (window as WindowWithSpeechRecognition).webkitSpeechRecognition;
  resetGeneratedClient();
});

describe('VoiceButton honours the admin Voice Features switches', () => {
  it('renders the control while voice is enabled', async () => {
    serveFlags(true, false);
    renderButton();
    const button = await screen.findByRole('button', { name: /voice input/i });
    expect(button).toBeEnabled();
  });

  it('renders NOTHING once an administrator switches voice off', async () => {
    serveFlags(false, false);
    renderButton();
    // The button is present first — the flags default to enabled while the
    // query is in flight — and must disappear once the answer lands. Asserting
    // immediately would pass on a component that never rendered it at all,
    // which is exactly what the previous case rules out.
    await waitFor(() => {
      expect(screen.queryByRole('button', { name: /voice input/i })).not.toBeInTheDocument();
    });
  });

  it('keeps the control VISIBLE but non-interactive when temporarily disabled', async () => {
    serveFlags(true, true);
    renderButton();
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /voice input/i })).toBeDisabled();
    });
    // Visible, disabled, and saying why — the state the "keep them visible"
    // switch exists for. A component that collapsed the two flags into one
    // would have hidden it instead.
    expect(screen.getByRole('button', { name: /voice input/i })).toBeInTheDocument();
  });
});

/**
 * #933 (ELITEA-1317): dictation splices at the last EDITED position, not at
 * a caret an incidental mouse click moved.
 */
describe('dictation insertion point (#933)', () => {
  function handle(overrides: Partial<VoiceButtonInputHandle>): VoiceButtonInputHandle {
    return {
      getInputContent: () => '',
      getCursorPosition: () => null,
      setValue: () => {},
      ...overrides,
    };
  }

  it('prefers the last edited position over the live caret', () => {
    const h = handle({ getCursorPosition: () => 5, getLastEditPosition: () => 11 });
    expect(resolveDictationInsertPoint(h, 'Hello world')).toBe(11);
  });

  it('falls back to the live caret when the composer has never been edited', () => {
    const h = handle({ getCursorPosition: () => 5, getLastEditPosition: () => null });
    expect(resolveDictationInsertPoint(h, 'Hello world')).toBe(5);
  });

  it('falls back to the live caret for a host whose handle does not track edits at all', () => {
    const h = handle({ getCursorPosition: () => 3 });
    expect(resolveDictationInsertPoint(h, 'Hello world')).toBe(3);
  });

  it('falls back to the end of the text when neither is known', () => {
    expect(resolveDictationInsertPoint(handle({}), 'Hello world')).toBe(11);
    expect(resolveDictationInsertPoint(null, 'Hello world')).toBe(11);
  });

  it('clamps a recorded position that outlived the text it described', () => {
    const h = handle({ getLastEditPosition: () => 99 });
    expect(resolveDictationInsertPoint(h, 'short')).toBe(5);
    expect(resolveDictationInsertPoint(handle({ getLastEditPosition: () => -4 }), 'short')).toBe(0);
  });

  it('separates a dictated phrase from the word it would otherwise weld onto', () => {
    expect(withDictationSeparator('Hello world')).toBe('Hello world ');
    expect(withDictationSeparator('Hello world ')).toBe('Hello world ');
    expect(withDictationSeparator('Hello world\n')).toBe('Hello world\n');
    expect(withDictationSeparator('')).toBe('');
  });
});
