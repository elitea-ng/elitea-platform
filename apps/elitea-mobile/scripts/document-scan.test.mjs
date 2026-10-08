// The document scan (issue #10, src/features/chat/attachments/scan/, native-modules/):
// static checks on what the unit tests cannot see. Run with `npm run test:scripts`.
//
// - Scans never reach the photo library and never leave the device for recognition: the
//   native code uses VisionKit, Vision, Core Graphics and Image I/O only.
// - The development sample pages (and their images) are required only behind `__DEV__`,
//   so Metro drops them from a release bundle.
// - The inline module's name agrees between Swift, JS and the app config.
import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { describe, it } from 'node:test';

const root = new URL('../', import.meta.url);
const read = (path) => readFileSync(new URL(path, root), 'utf8');
const swift = readdirSync(new URL('native-modules/', root))
  .filter((name) => name.endsWith('.swift'))
  .map((name) => ({ name, code: read(`native-modules/${name}`).replace(/\/\/.*$/gm, '') }));

function sourcesUnder(dir) {
  const out = [];
  for (const entry of readdirSync(new URL(dir, root), { withFileTypes: true })) {
    const path = `${dir}${entry.name}`;
    if (entry.isDirectory()) out.push(...sourcesUnder(`${path}/`));
    else if (/\.(ts|tsx)$/.test(entry.name)) out.push({ path, code: read(path) });
  }
  return out;
}

describe('document scan: native code', () => {
  it('finds the scanner module and its kit', () => {
    assert.deepEqual(swift.map((s) => s.name).sort(), ['DocumentScanKit.swift', 'DocumentScanner.swift']);
  });

  for (const { name, code } of swift) {
    it(`${name} never touches the photo library or the network`, () => {
      assert.doesNotMatch(code, /\bimport\s+Photos(UI)?\b|PHPhotoLibrary|UIImageWriteToSavedPhotosAlbum|PHAsset/);
      assert.doesNotMatch(code, /URLSession|URLRequest|NWConnection/);
    });
  }

  it('the module name agrees between Swift, JS and the app config', () => {
    const module = swift.find((s) => s.name === 'DocumentScanner.swift').code;
    assert.match(module, /final class DocumentScanner: Module/);
    assert.match(module, /Name\("DocumentScanner"\)/);
    assert.match(read('src/features/chat/attachments/scan/scanner.ts'), /load\('DocumentScanner'\)/);
    assert.match(read('app.config.ts'), /watchedDirectories: \[[^\]]*'native-modules'[^\]]*\]/);
  });
});

describe('document scan: file protection at rest', () => {
  it('every scan output is written natively with Complete protection: pages, PDF and text', () => {
    const module = read('native-modules/DocumentScanner.swift').replace(/\/\/.*$/gm, '');
    assert.match(module, /data\.write\(to: url, options: \[\.atomic, \.completeFileProtection\]\)/);
    assert.match(module, /AsyncFunction\("makePdf"\)[\s\S]*?FileProtectionType\.complete/);
    assert.match(module, /AsyncFunction\("writeText"\)[\s\S]*?DocumentScanKit\.writeProtectedText/);
    assert.match(read('native-modules/DocumentScanKit.swift'), /static func writeProtectedText[\s\S]*?\.completeFileProtection/);
  });

  it('the JS side never writes a scan file itself (expo-file-system gives the default class)', () => {
    const hook = read('src/features/chat/attachments/scan/useDocumentScan.ts');
    assert.doesNotMatch(hook, /\.write\(/);
    assert.match(read('src/features/chat/attachments/scan/scanPipeline.ts'), /deps\.native\.writeText\(/);
  });
});

describe('document scan: development sample pages', () => {
  const sources = sourcesUnder('src/');

  it('only useDocumentScan.ts requires the sample pages, behind __DEV__', () => {
    const users = sources.filter((s) => /scanSamples/.test(s.code) && !/scanSamples\.ts$/.test(s.path));
    assert.deepEqual(
      users.map((s) => s.path),
      ['src/features/chat/attachments/scan/useDocumentScan.ts'],
    );
    const hook = users[0].code;
    assert.doesNotMatch(hook, /^import .*scanSamples/m);
    assert.match(hook, /__DEV__ \? require\('@\/features\/chat\/attachments\/scan\/scanSamples'\)/);
  });

  it('nothing else requires the sample page images', () => {
    const users = sources.filter((s) => /assets\/dev\/scan\//.test(s.code)).map((s) => s.path);
    assert.deepEqual(users, ['src/features/chat/attachments/scan/scanSamples.ts']);
  });
});
