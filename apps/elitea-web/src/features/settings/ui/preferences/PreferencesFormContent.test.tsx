/**
 * Behavioural coverage for Settings > Preferences.
 *
 * The two things this page is *for* are (a) that each control persists
 * itself — there is no save button to fall back on — and (b) that the
 * Sound Notifications sub-controls are gated on the toggle. Both are
 * asserted through the real `localStorage` keys the rest of the app reads
 * (`el.notifications.sound-config`, `el.chat-input.voice-config`), not
 * through component state, because a page that renders the right thing
 * while writing to the wrong key is exactly the defect
 * `shared/lib/hooks/useSoundNotification.ts` records.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { beforeEach, describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { STORAGE_NAMESPACE } from '@/shared/lib/storage';

import { PreferencesFormContent } from './PreferencesFormContent';

const SOUND_KEY = `${STORAGE_NAMESPACE}notifications.sound-config`;
const VOICE_KEY = `${STORAGE_NAMESPACE}chat-input.voice-config`;

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderPage(): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <ThemeProvider
      theme={theme}
      defaultMode={DEFAULT_COLOR_SCHEME}
    >
      <CssBaseline />
      <QueryClientProvider client={queryClient}>
        <SocketClientContext.Provider value={createTestSocketClient()}>
          <PreferencesFormContent projectId="1" />
        </SocketClientContext.Provider>
      </QueryClientProvider>
    </ThemeProvider>,
  );
}

/** The stored sound config, or `null` when nothing has been written yet. */
function readSoundConfig(): { enabled?: boolean; volume?: number } | null {
  const raw = window.localStorage.getItem(SOUND_KEY);
  return raw === null ? null : (JSON.parse(raw) as { enabled?: boolean; volume?: number });
}

/** The stored voice config, or `null` when nothing has been written yet. */
function readVoiceConfig(): { rate?: number; volume?: number } | null {
  const raw = window.localStorage.getItem(VOICE_KEY);
  return raw === null ? null : (JSON.parse(raw) as { rate?: number; volume?: number });
}

/** The Sound Notifications toggle — matched on its own aria-label, not on position. */
function soundToggle(): HTMLElement {
  // `BaseSwitch` renders MUI's `Switch`, whose input carries role="switch".
  return screen.getByRole('switch', { name: /play sound when tasks complete/i });
}

describe('PreferencesFormContent', () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it('renders the three preference sections', () => {
    renderPage();

    expect(screen.getByTestId('preferences-general-section')).toBeInTheDocument();
    expect(screen.getByTestId('voice-personalization-section')).toBeInTheDocument();
    /*
     * TWO nodes now read "Sound Notifications": the accordion's own header and
     * the title of the toggle card inside it. That is what production renders
     * (the card is the same `title + description + switch` shape Memory uses
     * three times), so the assertion counts them instead of demanding one.
     */
    expect(screen.getAllByText(/^sound notifications$/i)).toHaveLength(2);
  });

  it('renders the theme toggle in the General section with no API call', () => {
    renderPage();

    const general = screen.getByTestId('preferences-general-section');
    expect(general).toHaveTextContent(/theme/i);
    expect(screen.getByRole('button', { name: /dark/i })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /light/i })).toBeInTheDocument();
  });

  describe('sound notifications', () => {
    it('defaults to enabled with the sub-controls visible before anything is stored', () => {
      renderPage();

      expect(soundToggle()).toBeChecked();
      expect(screen.getByRole('button', { name: /preview sound/i })).toBeInTheDocument();
      expect(readSoundConfig()).toBeNull();
    });

    it('hides the volume slider and preview button while disabled, and persists the toggle', async () => {
      const user = userEvent.setup();
      renderPage();

      await user.click(soundToggle());

      expect(soundToggle()).not.toBeChecked();
      expect(screen.queryByRole('button', { name: /preview sound/i })).not.toBeInTheDocument();
      expect(readSoundConfig()).toMatchObject({ enabled: false, volume: 0.5 });
    });

    it('restores a stored disabled state on mount', () => {
      window.localStorage.setItem(SOUND_KEY, JSON.stringify({ enabled: false, volume: 0.25 }));

      renderPage();

      expect(soundToggle()).not.toBeChecked();
      expect(screen.queryByRole('button', { name: /preview sound/i })).not.toBeInTheDocument();
    });

    it('re-shows the sub-controls when the toggle is turned back on', async () => {
      const user = userEvent.setup();
      window.localStorage.setItem(SOUND_KEY, JSON.stringify({ enabled: false, volume: 0.25 }));
      renderPage();

      await user.click(soundToggle());

      expect(soundToggle()).toBeChecked();
      expect(screen.getByRole('button', { name: /preview sound/i })).toBeInTheDocument();
      expect(readSoundConfig()).toMatchObject({ enabled: true, volume: 0.25 });
    });

    it('persists a volume change to the shared sound key', () => {
      renderPage();

      // The Volume slider inside the Sound Notifications panel is the one
      // whose max is 1 and whose current value is the 0.5 default; the voice
      // panel's volume slider defaults to 1.
      const sliders = screen.getAllByRole('slider');
      const soundVolume = sliders.find((s) => s.getAttribute('aria-valuenow') === '0.5');
      expect(soundVolume).toBeDefined();

      // Driven through the hidden native input rather than a pointer drag:
      // jsdom implements no `hasPointerCapture`, which MUI's `Slider` calls
      // on every pointer-down.
      fireEvent.change(soundVolume as HTMLElement, { target: { value: '1' } });

      const stored = readSoundConfig();
      expect(stored?.enabled).toBe(true);
      expect(stored?.volume).toBe(1);
      expect((soundVolume as HTMLElement).getAttribute('aria-valuenow')).toBe('1');
    });
  });

  describe('voice personalization', () => {
    it('restores stored rate and volume on mount', () => {
      window.localStorage.setItem(VOICE_KEY, JSON.stringify({ voiceName: null, voiceId: null, rate: 1.5, volume: 0 }));

      renderPage();

      const values = screen.getAllByRole('slider').map((s) => s.getAttribute('aria-valuenow'));
      expect(values).toContain('1.5');
    });

    it('persists a speed change to the shared voice key', () => {
      renderPage();

      // The Speed slider is the only one whose min is 0.5.
      const speed = screen.getAllByRole('slider').find((s) => s.getAttribute('aria-valuemin') === '0.5');
      expect(speed).toBeDefined();

      fireEvent.change(speed as HTMLElement, { target: { value: '2' } });

      const stored = readVoiceConfig();
      expect(stored).not.toBeNull();
      expect(stored?.rate).toBe(2);
    });
  });
});
