import { convertFormDataAsync } from 'expo/src/winter/fetch/convertFormData';

import { nativePart } from '@/api/attachments/upload';

/**
 * The default file part must be one the app's real fetch can send. Expo SDK 58
 * installs `expo/fetch` as the global fetch, and its multipart encoder
 * (convertFormDataAsync) accepts strings, Blobs and objects with `bytes()` —
 * NOT React Native's proprietary `{uri, name, type}` part, which it refuses
 * with "Unsupported FormDataPart implementation" before any byte leaves the
 * device. Every other upload test injects its own part, so only this one
 * exercises the part the app actually ships.
 */

const mockDisk: Record<string, Uint8Array> = {};
jest.mock('expo-file-system', () => ({
  File: class {
    mockUri: string;
    constructor(fileUri: string) {
      this.mockUri = fileUri;
    }
    async bytes() {
      const bytes = mockDisk[this.mockUri];
      if (!bytes) throw new Error(`no such file: ${this.mockUri}`);
      return bytes;
    }
  },
  Directory: jest.fn(),
  Paths: { cache: 'file:///cache' },
}));

/**
 * On the device a FormData yields each appended value as is (React Native's
 * FormData with expo's patch: `entries()` walks `_parts`), and the encoder
 * reads nothing but `entries()`. Jest's global FormData is Node's, which turns
 * an object part into "[object Object]", so the device's shape is built here.
 */
function deviceForm(parts: [string, unknown][]): FormData {
  return { entries: () => parts[Symbol.iterator]() } as unknown as FormData;
}

it('encodes the default part with the upload name, its type and the file bytes', async () => {
  mockDisk['file:///cache/ImageManipulator/EB43.jpg'] = new TextEncoder().encode('JPEGBYTES');
  const form = deviceForm([
    ['file', nativePart('file:///cache/ImageManipulator/EB43.jpg', 'zefir-photo-a4fd.jpg', 'image/jpeg')],
    ['file_name', 'zefir-photo-a4fd.jpg'],
  ]);

  const { body } = await convertFormDataAsync(form, 'BOUNDARY');
  const text = new TextDecoder().decode(body);

  expect(text).toContain('content-disposition: form-data; name="file"; filename="zefir-photo-a4fd.jpg"\r\n');
  expect(text).toContain('content-type: image/jpeg\r\n\r\nJPEGBYTES\r\n');
  expect(text).toContain('name="file_name"\r\n\r\nzefir-photo-a4fd.jpg\r\n');
});
