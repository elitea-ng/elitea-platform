import { isPickerCopy } from '@/features/chat/attachments/pickers';

jest.mock('expo-file-system', () => ({ File: jest.fn(), Directory: jest.fn(), Paths: { cache: { uri: 'file:///app/Library/Caches/' } } }));
jest.mock('expo-image-picker', () => ({}));
jest.mock('expo-document-picker', () => ({}));

const CACHE = 'file:///app/Library/Caches/';

describe('isPickerCopy', () => {
  it("is true only for the pickers' own copies in the app's cache", () => {
    // As the simulator stored them on the 4.2.0 run.
    expect(isPickerCopy(`${CACHE}ImagePicker/3D273A41-D1A9-41A5-B913-0542ADA9B7D3.jpeg`, CACHE)).toBe(true);
    expect(isPickerCopy(`${CACHE}DocumentPicker/E39F752C-FD0B-4DA0-960A-FAEC47F5C479.txt`, CACHE)).toBe(true);
    expect(isPickerCopy(`${CACHE}ImageManipulator/EB43CD09.jpg`, CACHE)).toBe(true);
    expect(isPickerCopy(`${CACHE}ImagePicker/`, CACHE)).toBe(false);
    expect(isPickerCopy(`${CACHE}att-tmp/x/notes.txt`, CACHE)).toBe(false);
    expect(isPickerCopy(`${CACHE}ImagePicker/../SQLite/db`, CACHE)).toBe(false);
    expect(isPickerCopy('file:///elsewhere/ImagePicker/a.jpg', CACHE)).toBe(false);
    expect(isPickerCopy('ph://ABCD', CACHE)).toBe(false);
    // The default reads the cache from expo-file-system.
    expect(isPickerCopy(`${CACHE}ImagePicker/a.jpg`)).toBe(true);
  });
});
