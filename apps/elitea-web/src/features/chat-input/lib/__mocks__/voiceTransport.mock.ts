/**
 * Replaces the two network calls of `shared/api/voiceTransport.ts` with spies a test
 * answers by hand (R-M1: `vi.mock` lives only under `__mocks__/`). Everything
 * else in that module — the error class, the abort check — stays real.
 *
 * Import this module BEFORE the module under test.
 */
import { vi } from 'vitest';

import type { SpeechRequest, TranscriptionRequest } from '@/shared/api/voiceTransport';

export const voiceTransportMock = {
  synthesizeSpeech: vi.fn<(request: SpeechRequest, signal?: AbortSignal) => Promise<ArrayBuffer>>(),
  transcribeAudio: vi.fn<(request: TranscriptionRequest, signal?: AbortSignal) => Promise<string>>(),
};

vi.mock('@/shared/api/voiceTransport', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/shared/api/voiceTransport')>()),
  synthesizeSpeech: (request: SpeechRequest, signal?: AbortSignal) => voiceTransportMock.synthesizeSpeech(request, signal),
  transcribeAudio: (request: TranscriptionRequest, signal?: AbortSignal) => voiceTransportMock.transcribeAudio(request, signal),
}));
