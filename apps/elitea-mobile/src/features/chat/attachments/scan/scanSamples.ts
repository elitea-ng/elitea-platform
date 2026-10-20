import { Directory, File } from 'expo-file-system';
import { Image } from 'react-native';

/**
 * DEVELOPMENT BUILDS ONLY (required behind `__DEV__` in useDocumentScan.ts, so a release
 * bundle carries neither this module nor its images): three sample pages of a field
 * inspection report, copied where the document camera would have written its pages, so the
 * rest of the pipeline (PDF, on-device text recognition, upload) runs unchanged on the
 * simulator, which has no camera.
 */
const SAMPLE_PAGES = [
  require('../../../../../assets/dev/scan/page-1.jpg'),
  require('../../../../../assets/dev/scan/page-2.jpg'),
  require('../../../../../assets/dev/scan/page-3.jpg'),
];

export async function samplePages(directory: string): Promise<string[]> {
  const dir = new Directory(directory);
  dir.create({ intermediates: true, idempotent: true });
  const pages: string[] = [];
  for (const [index, asset] of SAMPLE_PAGES.entries()) {
    const source = Image.resolveAssetSource(asset)?.uri;
    if (!source) throw new Error(`sample page ${index + 1} is not in the bundle`);
    const page = new File(dir, `page-${index + 1}.jpg`);
    // Metro serves assets over HTTP in development; an embedded bundle has them as files.
    if (/^https?:/.test(source)) await File.downloadFileAsync(source, page, { idempotent: true });
    else new File(source).copy(page);
    pages.push(page.uri);
  }
  return pages;
}
