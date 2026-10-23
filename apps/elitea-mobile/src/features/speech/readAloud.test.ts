import * as Speech from 'expo-speech';

import { answerGroupBefore } from '@/api/__fixtures__/chat13';
import { endSession } from '@/security/endSession';
import { readAloud, spokenText, stopReading, useReading } from '@/features/speech/readAloud';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const speak = Speech.speak as jest.Mock;
beforeEach(() => {
  speak.mockReset();
  (Speech.stop as jest.Mock).mockClear();
});
void useReading;

describe('read aloud (on-device text to speech)', () => {
  it('announces a code block instead of reading it, and drops Markdown marks', () => {
    expect(spokenText(answerGroupBefore.content)).toBe('MOCK: Show me code:\n Code block. \nthanks');
    expect(spokenText('# Title\n\n**Bold** and _it_ with [a link](https://x.test) and `code`.\n\n- one\n- two')).toBe(
      'Title\n\nBold and it with a link and code.\n\none\ntwo',
    );
    expect(spokenText('call mock_tool_status and *stress* it')).toBe('call mock_tool_status and stress it');
  });

  it('speaks the answer, and stops it', () => {
    readAloud('m1', '**Hello** there');
    expect(speak).toHaveBeenCalledWith('Hello there', expect.objectContaining({ language: expect.any(String) }));
    stopReading();
    expect(Speech.stop).toHaveBeenCalled();
  });

  it('a new reading stops the one before; an empty text reads nothing', () => {
    readAloud('m1', 'one');
    readAloud('m2', 'two');
    expect(Speech.stop).toHaveBeenCalledTimes(2);
    speak.mockClear();
    readAloud('m3', '```\nonly code\n```'.replace('only code', ''));
    expect(speak).toHaveBeenCalledTimes(1);
  });

  it('a wipe stops reading', async () => {
    readAloud('m1', 'one');
    (Speech.stop as jest.Mock).mockClear();
    await endSession({ reason: 'user_logout' });
    expect(Speech.stop).toHaveBeenCalled();
  });
});
