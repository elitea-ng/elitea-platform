import { documentScanner, scanSource, type DocumentScannerNative } from '@/features/chat/attachments/scan/scanner';
import { attachSources } from '@/features/chat/attachments/AttachmentTrayView';


const native = (isSupported: boolean, isSimulator = false) => ({ isSupported, isSimulator }) as DocumentScannerNative;

describe('scanSource', () => {
  it('is the document camera where VisionKit can scan', () => {
    expect(scanSource(native(true), false)).toBe('camera');
    expect(scanSource(native(true), true)).toBe('camera');
  });

  it('is the sample pages only in a development build without a camera (the simulator)', () => {
    expect(scanSource(native(false), true)).toBe('samples');
    expect(scanSource(native(false), false)).toBeNull();
  });

  it('never the camera on the simulator, where VisionKit says yes but cannot capture', () => {
    expect(scanSource(native(true, true), true)).toBe('samples');
    expect(scanSource(native(true, true), false)).toBeNull();
  });

  it('is not offered without the native module', () => {
    expect(scanSource(null, true)).toBeNull();
  });
});

describe('documentScanner', () => {
  it('is the DocumentScanner inline module on iOS', () => {
    const load = jest.fn(() => native(true));
    expect(documentScanner('ios', load)).toEqual(native(true));
    expect(load).toHaveBeenCalledWith('DocumentScanner');
  });

  it('is absent where the module is not built in (Expo Go)', () => {
    expect(documentScanner('ios', () => null)).toBeNull();
  });

  it('is absent on Android until the ML Kit scanner lands (#20)', () => {
    const load = jest.fn(() => native(true));
    expect(documentScanner('android', load)).toBeNull();
    expect(load).not.toHaveBeenCalled();
  });
});

describe('the attach menu', () => {
  it('lists "Scan document" between Camera and Files only where a scan can run', () => {
    expect(attachSources(null).map((s) => s.label)).toEqual(['Photo library', 'Camera', 'Files']);
    expect(attachSources('camera').map((s) => s.label)).toEqual(['Photo library', 'Camera', 'Scan document', 'Files']);
    expect(attachSources('samples').map((s) => s.label)).toEqual(['Photo library', 'Camera', 'Scan document (sample pages)', 'Files']);
    expect(attachSources('camera')[2]).toMatchObject({ source: 'scan', icon: 'scanDocument' });
  });
});
