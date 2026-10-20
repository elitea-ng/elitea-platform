import { holdsOnlySent, isKeyboardCommit, KEYBOARD_COMMIT_WINDOW_MS } from '@/features/chat/keyboardCommit';

describe('isKeyboardCommit (#56)', () => {
  it.each([
    ['hello zefir', 'hello Zefir'],
    ['hello zefir', 'hello Zefir '],
    ['hello zefir ', 'hello Zefir '],
    ['see you tomorow', 'see you tomorrow'],
    ['i want alot', 'i want a lot'],
    ['cafe', 'café'],
    ['teh', 'the'],
  ])('%p → %p is the keyboard finishing the last word', (sent, current) => {
    expect(isKeyboardCommit(sent, current)).toBe(true);
  });

  it.each([
    ['hello zefir', 'hello zefir and more'],
    ['hello zefir', 'goodbye zefir'],
    ['hello zefir', 'hello'],
    ['hello zefir', ''],
    ['hello zefir', 'hello completelydifferent'],
    ['', 'typed'],
  ])('%p → %p is the user', (sent, current) => {
    expect(isKeyboardCommit(sent, current)).toBe(false);
  });
});

describe('holdsOnlySent', () => {
  it('only a commit right after Send counts as the keyboard', () => {
    expect(holdsOnlySent('hi zefir', 'hi zefir', 0, null)).toBe(true);
    expect(holdsOnlySent('hi zefir', 'hi Zefir', 1000, 1000 + 50)).toBe(true);
    expect(holdsOnlySent('hi zefir', 'hi Zefir', 1000, 1000 + KEYBOARD_COMMIT_WINDOW_MS + 1)).toBe(false);
    expect(holdsOnlySent('hi zefir', 'hi Zefir', 1000, null)).toBe(false);
  });
});
