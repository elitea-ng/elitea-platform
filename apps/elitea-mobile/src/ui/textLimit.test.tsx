import { render, screen } from '@testing-library/react-native';

import { canSaveName } from '@/features/chats/RenameView';
import { DESCRIPTION_MAX_LENGTH, exceedsLimit, limitMessage, NAME_MAX_LENGTH, TextField } from '@/ui';

jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);

describe('entity text limits (issue 1968)', () => {
  it('are the contract numbers, counted in characters', () => {
    expect(NAME_MAX_LENGTH).toBe(128);
    expect(DESCRIPTION_MAX_LENGTH).toBe(2000);
    expect(exceedsLimit('a'.repeat(128), NAME_MAX_LENGTH)).toBe(false);
    expect(exceedsLimit('a'.repeat(129), NAME_MAX_LENGTH)).toBe(true);
    expect(exceedsLimit('🙂'.repeat(128), NAME_MAX_LENGTH)).toBe(false);
    expect(exceedsLimit('字'.repeat(2001), DESCRIPTION_MAX_LENGTH)).toBe(true);
  });

  it('says nothing early, counts down near the limit and errors past it', () => {
    expect(limitMessage('abc', 128)).toEqual({ note: null, error: null });
    expect(limitMessage('a'.repeat(120), 128).note).toBe('8 characters left');
    expect(limitMessage('a'.repeat(130), 128).error).toBe('2 characters over the limit. Shorten the text to save.');
  });

  it('blocks the rename sheet past the limit', () => {
    expect(canSaveName('a'.repeat(128), 'old')).toBe(true);
    expect(canSaveName('a'.repeat(129), 'old')).toBe(false);
  });

  it('TextField keeps the text whole and shows the overflow', async () => {
    await render(<TextField label="Name" value={'a'.repeat(129)} onChangeText={() => {}} characterLimit={128} />);
    expect(screen.getByDisplayValue('a'.repeat(129))).toBeTruthy();
    expect(screen.getByText('1 character over the limit. Shorten the text to save.')).toBeTruthy();
  });
});
