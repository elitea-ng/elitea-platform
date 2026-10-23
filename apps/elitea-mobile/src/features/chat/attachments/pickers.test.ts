import { isPickerCopy, sweepPickerCopies } from '@/features/chat/attachments/pickers';
import { beginScan } from '@/features/chat/attachments/scan/scanInFlight';

jest.mock('expo-file-system', () => {
  const removed: string[] = [];
  return {
    __removed: removed,
    File: jest.fn(),
    Directory: jest.fn().mockImplementation((_base: unknown, name: string) => ({ exists: true, delete: () => removed.push(name) })),
    Paths: { cache: { uri: 'file:///app/Library/Caches/' } },
  };
});
jest.mock('expo-image-picker', () => ({}));
jest.mock('expo-document-picker', () => ({}));

const CACHE = 'file:///app/Library/Caches/';

describe('isPickerCopy', () => {
  it("is true only for the pickers' own copies in the app's cache", () => {
    // As the simulator stored them on the 4.2.0 run.
    expect(isPickerCopy(`${CACHE}ImagePicker/3D273A41-D1A9-41A5-B913-0542ADA9B7D3.jpeg`, CACHE)).toBe(true);
    expect(isPickerCopy(`${CACHE}DocumentPicker/E39F752C-FD0B-4DA0-960A-FAEC47F5C479.txt`, CACHE)).toBe(true);
    expect(isPickerCopy(`${CACHE}ImageManipulator/EB43CD09.jpg`, CACHE)).toBe(true);
    // The document scan's PDF or text, in its own directory per scan.
    expect(isPickerCopy(`${CACHE}DocumentScan/6F1C2B9E-0D4A-4C55-9A57-1E0C4B1E2F3A/scan.pdf`, CACHE)).toBe(true);
    expect(isPickerCopy(`${CACHE}DocumentScan/../SQLite/db`, CACHE)).toBe(false);
    expect(isPickerCopy(`${CACHE}ImagePicker/`, CACHE)).toBe(false);
    expect(isPickerCopy(`${CACHE}att-tmp/x/notes.txt`, CACHE)).toBe(false);
    expect(isPickerCopy(`${CACHE}ImagePicker/../SQLite/db`, CACHE)).toBe(false);
    expect(isPickerCopy('file:///elsewhere/ImagePicker/a.jpg', CACHE)).toBe(false);
    expect(isPickerCopy('ph://ABCD', CACHE)).toBe(false);
    // The default reads the cache from expo-file-system.
    expect(isPickerCopy(`${CACHE}ImagePicker/a.jpg`)).toBe(true);
  });
});

describe('sweepPickerCopies', () => {
  const removed = jest.requireMock<{ __removed: string[] }>('expo-file-system').__removed;
  beforeEach(() => removed.splice(0));

  it('deletes every picker directory', () => {
    sweepPickerCopies();
    expect(removed).toEqual(['ImagePicker', 'DocumentPicker', 'ImageManipulator', 'DocumentScan', 'com.apple.DocumentCamera']);
  });

  it("leaves VisionKit's scratch alone while a scan runs (its camera may be using it); the scan sweeps it when it ends (F3)", () => {
    const end = beginScan();
    expect(end).not.toBeNull();
    try {
      sweepPickerCopies();
      expect(removed).not.toContain('com.apple.DocumentCamera');
      expect(removed).toContain('DocumentScan');
    } finally {
      end?.();
    }
  });
});
