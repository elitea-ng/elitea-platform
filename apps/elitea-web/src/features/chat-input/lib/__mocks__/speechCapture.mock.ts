/**
 * Replaces the microphone (`helpers/speechCapture.ts`'s `startSpeechCapture`)
 * with a spy, so a test hands frames straight to the frame callback (R-M1:
 * `vi.mock` lives only under `__mocks__/`). The pure helpers in that module
 * stay real.
 *
 * Import this module BEFORE the module under test.
 */
import { vi } from 'vitest';

import type { SpeechCapture } from '../helpers/speechCapture';

export const speechCaptureMock = {
  startSpeechCapture: vi.fn<(onFrame: (frame: Float32Array) => void) => Promise<SpeechCapture>>(),
};

vi.mock('../helpers/speechCapture', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../helpers/speechCapture')>()),
  startSpeechCapture: (onFrame: (frame: Float32Array) => void) => speechCaptureMock.startSpeechCapture(onFrame),
}));
