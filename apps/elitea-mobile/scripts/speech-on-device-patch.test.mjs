// Dictation's "never sent off the device" promise (src/features/speech/dictation.ts)
// rests on a patch to expo-speech-recognition's iOS code: unpatched, the library
// sets requiresOnDeviceRecognition only when the requested language has an
// on-device model, and otherwise streams the audio to Apple's servers without a
// word (SEC-2). Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, it } from 'node:test';

const root = new URL('..', import.meta.url);
const read = (path) => readFileSync(new URL(path, root), 'utf8');
const ios = 'node_modules/expo-speech-recognition/ios/';

describe('expo-speech-recognition on-device patch', () => {
  it('pins the exact version the patch was written against', () => {
    const pkg = JSON.parse(read('package.json'));
    const version = pkg.dependencies['expo-speech-recognition'];
    assert.match(version, /^\d+\.\d+\.\d+$/, 'an exact version, never a range');
    assert.equal(JSON.parse(read('node_modules/expo-speech-recognition/package.json')).version, version);
    assert.ok(read(`patches/expo-speech-recognition+${version}.patch`).length > 0);
    assert.match(pkg.scripts.postinstall ?? '', /patch-package/);
  });

  it('is applied: start() refuses a language without an on-device model', () => {
    const module = read(`${ios}ExpoSpeechRecognitionModule.swift`);
    assert.ok(/options\.requiresOnDeviceRecognition,\s*await self\.speechRecognizer\?\.supportsOnDeviceRecognition\(\) != true/.test(module), 'no per-language on-device guard in start()');
    assert.ok(/error: "language-not-supported",\s*message: "Locale [^"]*on-device/.test(module));
  });

  it('is applied: the request always carries requiresOnDeviceRecognition', () => {
    const recognizer = read(`${ios}ExpoSpeechRecognizer.swift`);
    assert.ok(!/if recognizer\.supportsOnDeviceRecognition \{\s*request\.requiresOnDeviceRecognition/.test(recognizer), 'the flag is still conditional');
    assert.ok(/request\.requiresOnDeviceRecognition = options\.requiresOnDeviceRecognition/.test(recognizer));
  });
});
