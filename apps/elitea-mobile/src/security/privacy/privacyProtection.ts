import * as ScreenCapture from 'expo-screen-capture';

import { log } from '@/lib/log';
import type { ClientPolicy } from '@/security/policy';

/**
 * Screen privacy, driven by the client policy.
 *
 * - iOS: the native app-switcher blur (`enableAppSwitcherProtectionAsync`,
 *   intensity 0.9) is enabled once at startup, whatever the policy. When the
 *   policy disallows screenshots, `preventScreenCaptureAsync` also blanks
 *   screenshots and recordings (iOS 13+).
 * - Android: `preventScreenCaptureAsync` sets FLAG_SECURE, which blocks
 *   screenshots AND blanks the Recents thumbnail. It is on while
 *   `allow_screenshots` is false (the strict default). When the policy allows
 *   screenshots it is released, and the in-app cover (`PrivacyCover`) is the
 *   only, best-effort protection: Android may take the Recents snapshot before
 *   JS renders the cover. Follow-up: a config plugin for
 *   `setRecentsScreenshotEnabled(false)` (API 33+).
 * - Web: nothing (development surface).
 *
 * Every call is idempotent per state and never throws.
 */

export const SCREEN_CAPTURE_KEY = 'policy';
export const APP_SWITCHER_BLUR = 0.9;

export type ScreenCaptureApi = Pick<
  typeof ScreenCapture,
  'preventScreenCaptureAsync' | 'allowScreenCaptureAsync' | 'enableAppSwitcherProtectionAsync'
>;

export class PrivacyProtection {
  private switcherProtected = false;
  private captureBlocked: boolean | null = null;
  private queue: Promise<void> = Promise.resolve();

  constructor(
    private readonly platform: string,
    private readonly api: ScreenCaptureApi = ScreenCapture,
  ) {}

  /** Applies `policy`. Calls are serialised, so a quick toggle ends in the last state. */
  apply(policy: Pick<ClientPolicy, 'allow_screenshots'>): Promise<void> {
    this.queue = this.queue.then(() => this.applyNow(policy));
    return this.queue;
  }

  private async applyNow(policy: Pick<ClientPolicy, 'allow_screenshots'>): Promise<void> {
    if (this.platform !== 'ios' && this.platform !== 'android') return;

    if (this.platform === 'ios' && !this.switcherProtected) {
      try {
        await this.api.enableAppSwitcherProtectionAsync(APP_SWITCHER_BLUR);
        this.switcherProtected = true;
      } catch (error) {
        log.warn('app-switcher protection unavailable', { error });
      }
    }

    const block = !policy.allow_screenshots;
    if (this.captureBlocked === block) return;
    try {
      if (block) await this.api.preventScreenCaptureAsync(SCREEN_CAPTURE_KEY);
      else await this.api.allowScreenCaptureAsync(SCREEN_CAPTURE_KEY);
      this.captureBlocked = block;
    } catch (error) {
      log.warn('screen capture policy could not be applied', { error, block });
    }
  }
}

/** Whether the in-app cover is shown for an AppState value: everything except 'active'. */
export function coverVisible(state: string | null | undefined): boolean {
  return state !== 'active' && state !== null && state !== undefined;
}
