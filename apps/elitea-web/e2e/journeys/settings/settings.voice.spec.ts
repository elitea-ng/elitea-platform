/**
 * Voice / text-to-speech personalization — the platform's OWN controls.
 *
 * Ported from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/voice/
 * test_voice_configuration.py`), by use case:
 *
 *   V1 ← `test_voice_selection_from_chat`      (TC1)
 *   V2 ← `test_speed_and_volume_controls`      (TC2)
 *   V3 ← `test_voice_settings_sync`            (TC3)
 *   V4 ← `test_voice_preview_personalization`  (TC4)
 *
 * The fifth legacy case, `test_voice_settings_not_visible_by_default` (TC5),
 * is ported into `admin.features.spec.ts`'s J36f instead: it is about the
 * platform switch, and J36f already holds the platform-flag lock that
 * flipping that switch needs.
 *
 * ## Why every one of them lands on Settings › Personalization
 *
 * The legacy suite drives the CHAT-side TTS dialog: read-out button → gear →
 * "Voice settings". This platform has that dialog
 * (`features/chat-input/ui/VoiceConfigDialog.tsx`) and it has the mini player
 * that opens it (`VoiceMiniPlayer` / `VoiceControlButton`) — and NOTHING
 * MOUNTS EITHER. `VoiceControlButton.tsx`'s own module doc says so in as many
 * words: "This component has no render site today — it and `VoiceMiniPlayer`
 * are exported from this slice's barrel and imported by nothing." The one
 * MOUNTED voice control on `/chat` is `widgets/chat`'s `VoiceButton`, which
 * is the microphone (ASR), not playback.
 *
 * So the voice/speed/volume controls the legacy tests are about exist on
 * exactly one screen here — `Settings › Personalization`'s Voice
 * Personalization section — and that is where they are asserted. Driving the
 * chat dialog would mean asserting a surface the product does not render,
 * which is the "journey that passes against nothing" shape this suite has
 * been bitten by.
 *
 * ## Why the browser voice list is stubbed
 *
 * `VoicePersonalizationSection` offers MODEL voices only while
 * `shouldUseModelVoices` holds, and `GET /configurations/tts_voices/{project}`
 * answers 501 for every project (#466) — so the list it actually renders is
 * `window.speechSynthesis.getVoices()`. A headless Chromium or WebKit on CI
 * ships no speech-dispatcher and answers `[]`, and the section then renders NO
 * Voice select at all (`voiceOptions.length > 0` gates it). A test that
 * shrugged at that would be reporting the runner's audio stack, not the
 * product. Two voices are installed instead, in an init script, so the control
 * under test is the same on both engines.
 *
 * `speechSynthesis.speak` is stubbed with them, and that is what makes V4 an
 * assertion rather than a click: the preview's whole observable effect is the
 * utterance it hands the engine.
 */
import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

/** `shared/lib/storage.ts`'s namespace + `voiceConfig.helpers.ts`'s `STORAGE_KEY`. */
const VOICE_CONFIG_KEY = 'el.chat-input.voice-config';

const PERSONALIZATION_URL = `${BASE_URL}/app/settings/personalization`;

/** The two voices every test in this file sees. `localService: false` is the "(online)" suffix branch. */
const LOCAL_VOICE = 'Autotest Alto';
const ONLINE_VOICE = 'Autotest Tenor';

/** What `voiceConfig.helpers.ts` stores. */
interface StoredVoiceConfig {
  readonly voiceName: string | null;
  readonly voiceId: string | null;
  readonly rate: number;
  readonly volume: number;
}

/** One `speechSynthesis.speak()` call, as the stub records it. */
interface SpokenUtterance {
  readonly text: string;
  readonly rate: number;
  readonly volume: number;
}

/**
 * Install a deterministic Web Speech API.
 *
 * `addInitScript` and not `evaluate`: the section reads `getVoices()` in its
 * first effect, so a stub installed after the document has loaded arrives
 * after the render that needed it.
 *
 * `Object.defineProperty` on `window` shadows the engine's own accessor,
 * which lives on `Window.prototype` and is not writable — the same shape
 * `VoicePersonalizationSection.test.tsx` uses for jsdom.
 */
async function installBrowserVoices(page: Page): Promise<void> {
  await page.addInitScript(
    ([localName, onlineName]: readonly string[]) => {
      const spoken: unknown[] = [];
      (window as unknown as { __autotestSpoken: unknown[] }).__autotestSpoken = spoken;

      class StubUtterance {
        text: string;
        rate = 1;
        volume = 1;
        voice: unknown = null;
        onend: (() => void) | null = null;
        onerror: (() => void) | null = null;
        constructor(text: string) {
          this.text = text;
        }
      }
      Object.defineProperty(window, 'SpeechSynthesisUtterance', {
        configurable: true,
        value: StubUtterance,
      });

      Object.defineProperty(window, 'speechSynthesis', {
        configurable: true,
        value: {
          getVoices: () => [
            { name: localName, localService: true, lang: 'en-US', default: true, voiceURI: localName },
            { name: onlineName, localService: false, lang: 'en-GB', default: false, voiceURI: onlineName },
          ],
          addEventListener: () => {},
          removeEventListener: () => {},
          cancel: () => {},
          speak: (utterance: StubUtterance) => {
            spoken.push({ text: utterance.text, rate: utterance.rate, volume: utterance.volume });
            // Synchronously, so the button leaves its loading state and the
            // panel is usable again — the real engine fires this when the
            // audio ends.
            utterance.onend?.();
          },
        },
      });
    },
    [LOCAL_VOICE, ONLINE_VOICE] as const,
  );
}

/** The stored preference, or `null` when the panel has written nothing yet. */
async function storedVoiceConfig(page: Page): Promise<StoredVoiceConfig | null> {
  const raw = await page.evaluate((key: string) => localStorage.getItem(key), VOICE_CONFIG_KEY);
  return raw === null ? null : (JSON.parse(raw) as StoredVoiceConfig);
}

async function spokenUtterances(page: Page): Promise<readonly SpokenUtterance[]> {
  return page.evaluate(
    () => (window as unknown as { __autotestSpoken?: SpokenUtterance[] }).__autotestSpoken ?? [],
  );
}

/**
 * Open the panel on a KNOWN starting state.
 *
 * The preference is per-browser-context `localStorage`, and a context is
 * seeded from the persona storage state — so a value another run wrote could
 * arrive with the session. Clearing it and reloading makes "the defaults are
 * 1× and 100%" a fact about the product rather than about the fixture.
 */
async function openVoicePanel(page: Page) {
  await page.goto(PERSONALIZATION_URL, { waitUntil: 'domcontentloaded' });
  await page.evaluate((key: string) => localStorage.removeItem(key), VOICE_CONFIG_KEY);
  await page.reload({ waitUntil: 'domcontentloaded' });

  const section = page.getByTestId('voice-personalization-section');
  await expect(section, 'the Personalization page must render the voice section').toBeVisible({
    timeout: 30_000,
  });
  return section;
}

test.beforeEach(async ({ page }) => {
  await installBrowserVoices(page);
});

test('V1: the voice list is offered and the chosen voice persists (legacy test_voice_selection_from_chat)', async ({
  page,
}) => {
  /*
   * THE BUDGET IS THE SUM OF THE WAITS BELOW. `openVoicePanel` loads the page
   * and reloads it, then waits up to 30 s for the section; the voice list gets
   * another 30 s, and three more waits follow. The default 30 s is smaller
   * than the first wait alone, so the test clock — not any assertion — ended
   * this run (webkit).
   */
  test.setTimeout(150_000);
  const section = await openVoicePanel(page);

  /*
   * The select renders only once the voice list has RESOLVED: while the
   * model-voice query is in flight `shouldUseModelVoices` is still true and
   * `displayVoices` is empty, so the control is unmounted (the same window
   * `VoicePersonalizationSection.test.tsx` had to settle around). Waiting for
   * the combobox is waiting for that.
   */
  const voiceSelect = section.getByRole('combobox');
  await expect(voiceSelect).toBeVisible({ timeout: 30_000 });

  await voiceSelect.click();

  // BOTH voices, and the "(online)" suffix — `toVoiceOptions` appends it for
  // a voice the engine reports as non-local, so this is the label the panel
  // built rather than the name the engine gave.
  await expect(page.getByRole('option', { name: LOCAL_VOICE, exact: true })).toBeVisible({
    timeout: 15_000,
  });
  const onlineOption = page.getByRole('option', { name: `${ONLINE_VOICE} (online)`, exact: true });
  await expect(onlineOption).toBeVisible();

  await onlineOption.click();

  // The panel wrote the choice, under the browser-voice field. `voiceId` is
  // the OTHER branch (model voices) and must stay cleared, or a later render
  // would resolve a model voice that does not exist here.
  await expect
    .poll(async () => (await storedVoiceConfig(page))?.voiceName, {
      timeout: 15_000,
      message: 'selecting a voice did not reach the stored preference',
    })
    .toBe(ONLINE_VOICE);
  expect((await storedVoiceConfig(page))?.voiceId).toBeNull();

  // …and it survives a fresh boot of the app, which is what makes it a
  // preference rather than component state.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(
    page.getByTestId('voice-personalization-section').getByRole('combobox'),
    'the stored voice must be the one the reloaded select shows',
  ).toContainText(`${ONLINE_VOICE} (online)`, { timeout: 30_000 });
});

test('V2: the speed and volume sliders move and their values persist (legacy test_speed_and_volume_controls)', async ({
  page,
}) => {
  const section = await openVoicePanel(page);

  const speed = section.getByRole('slider', { name: 'Speed' });
  const volume = section.getByRole('slider', { name: 'Volume' });
  await expect(speed).toBeVisible({ timeout: 30_000 });
  await expect(volume).toBeVisible();

  // The defaults `DEFAULT_CONFIG` declares — 1× and 100%.
  await expect(speed).toHaveAttribute('aria-valuenow', '1');
  await expect(volume).toHaveAttribute('aria-valuenow', '1');

  /*
   * DRIVEN BY KEYBOARD, not by a drag.
   *
   * `Home`/`End` land on the range's exact ends, so the assertion is `0.5`
   * and `2`, not "somewhere near the left". A mouse drag would have to
   * compute a pixel offset from the rail's bounding box and would then be
   * asserting arithmetic this test did itself. The control is a real
   * `input[type=range]` inside MUI's thumb, so the keys are the product's own
   * accessibility path as well.
   */
  await speed.press('Home');
  await expect(speed).toHaveAttribute('aria-valuenow', '0.5');
  await volume.press('Home');
  await expect(volume).toHaveAttribute('aria-valuenow', '0');

  await expect
    .poll(async () => await storedVoiceConfig(page), {
      timeout: 15_000,
      message: 'moving the sliders did not reach the stored preference',
    })
    .toMatchObject({ rate: 0.5, volume: 0 });

  // A full reload: the sliders come back on the STORED values, not on the
  // defaults. `loadStored`'s clamp is what a corrupted value would fall back
  // to, so a panel that failed to read would show 1× / 100% here.
  await page.reload({ waitUntil: 'domcontentloaded' });
  const reloaded = page.getByTestId('voice-personalization-section');
  await expect(reloaded.getByRole('slider', { name: 'Speed' })).toHaveAttribute(
    'aria-valuenow',
    '0.5',
    { timeout: 30_000 },
  );
  await expect(reloaded.getByRole('slider', { name: 'Volume' })).toHaveAttribute(
    'aria-valuenow',
    '0',
  );

  // The other end of both ranges, so the ports of the legacy 2×/100% steps
  // are covered too and a slider stuck at its minimum cannot pass.
  await reloaded.getByRole('slider', { name: 'Speed' }).press('End');
  await reloaded.getByRole('slider', { name: 'Volume' }).press('End');
  await expect
    .poll(async () => await storedVoiceConfig(page), { timeout: 15_000 })
    .toMatchObject({ rate: 2, volume: 1 });
});

test('V3: the preference the settings page writes is the one the chat slice reads (legacy test_voice_settings_sync)', async ({
  page,
}) => {
  const section = await openVoicePanel(page);

  const voiceSelect = section.getByRole('combobox');
  await expect(voiceSelect).toBeVisible({ timeout: 30_000 });
  await voiceSelect.click();
  await page.getByRole('option', { name: LOCAL_VOICE, exact: true }).click();
  await section.getByRole('slider', { name: 'Speed' }).press('Home');

  await expect
    .poll(async () => await storedVoiceConfig(page), { timeout: 15_000 })
    .toMatchObject({ voiceName: LOCAL_VOICE, rate: 0.5 });

  /*
   * THE SYNC THIS PLATFORM CAN ACTUALLY BE ASKED ABOUT.
   *
   * The legacy body sets the preference in Personalization, then opens the
   * chat TTS dialog and reads the same three values back out of it. That
   * dialog has no mount site here (see this file's header), so the second
   * half of that round trip has no surface to run on.
   *
   * What IS the mechanism behind it — and what would actually break the
   * legacy assertion if it regressed — is that both sides read ONE key.
   * `features/settings/.../voiceConfig.helpers.ts` and
   * `features/chat-input/lib/hooks/useVoiceConfig.hooks.ts` each declare
   * `STORAGE_KEY = 'chat-input.voice-config'` in their own file (a disclosed
   * near-duplicate, forced by `no-sideways-features`), and two copies of a
   * string are two chances for them to stop agreeing. So the key is asserted
   * by NAME, and asserted to be the only voice key the app wrote.
   */
  const voiceKeys = await page.evaluate(() =>
    Object.keys(localStorage).filter((key) => key.includes('voice')),
  );
  expect(voiceKeys, 'the panel must write exactly one voice preference key').toEqual([
    VOICE_CONFIG_KEY,
  ]);

  // It is readable from the chat route — the same origin, the same key, after
  // a real navigation rather than a re-render.
  await page.goto(`${BASE_URL}/app/chat`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
  expect(await storedVoiceConfig(page)).toMatchObject({ voiceName: LOCAL_VOICE, rate: 0.5 });
});

test('V4: Preview Voice speaks, at the chosen speed and volume (legacy test_voice_preview_personalization)', async ({
  page,
}) => {
  const section = await openVoicePanel(page);

  // Every control the legacy body inspects, on one screen.
  await expect(section.getByText('Voice Personalization')).toBeVisible({ timeout: 30_000 });
  await expect(section.getByRole('combobox')).toBeVisible();
  await expect(section.getByRole('slider', { name: 'Speed' })).toBeVisible();
  await expect(section.getByRole('slider', { name: 'Volume' })).toBeVisible();
  const preview = section.getByTestId('voice-preview-button');
  await expect(preview).toBeVisible();
  await expect(preview).toBeEnabled();

  // Nothing has been spoken yet, so the click below cannot be credited to an
  // earlier render.
  expect(await spokenUtterances(page)).toHaveLength(0);

  await section.getByRole('slider', { name: 'Speed' }).press('Home');
  await expect(section.getByRole('slider', { name: 'Speed' })).toHaveAttribute(
    'aria-valuenow',
    '0.5',
  );

  await preview.click();

  /*
   * THE UTTERANCE IS THE ASSERTION.
   *
   * `handlePreview` has a live branch and a NO-OP branch — it returns without
   * speaking whenever the panel committed to model voices — and the button
   * looks identical in both. Clicking it and checking it was clickable, which
   * is what the legacy body does, passes on the dead branch. What the engine
   * received does not.
   *
   * The VOICE is deliberately not asserted: `speakBrowserPreview` sets rate
   * and volume on the utterance and never sets `voice`, so the preview plays
   * the engine default. That is the product's behaviour today, disclosed
   * here rather than asserted as if it were correct.
   */
  await expect
    .poll(async () => (await spokenUtterances(page)).length, {
      timeout: 15_000,
      message: 'Preview Voice handed the speech engine nothing to say',
    })
    .toBe(1);
  const [utterance] = await spokenUtterances(page);
  expect(utterance?.rate, 'the preview must play at the speed the slider shows').toBe(0.5);
  expect(utterance?.volume).toBe(1);
  expect(utterance?.text.toLowerCase()).toContain('preview');
});
