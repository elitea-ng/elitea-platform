// @ts-check
const { withAppDelegate, withMainApplication } = require('expo/config-plugins');

/**
 * Agent Zefir keeps nothing it fetched outside the encrypted offline store.
 *
 * iOS's default URL sessions (React Native's networking module and
 * `expo/fetch` both use `URLSessionConfiguration.default`) write every
 * response, the native token response included, to the shared `URLCache`:
 * a plaintext SQLite file at `Library/Caches/<bundle id>/Cache.db`. The E2E
 * run found the access and refresh tokens and whole transcripts there, still
 * readable after "Log out & wipe" and after an admin revoke (DEF-R2), even
 * though the server answers `Cache-Control: no-store`.
 *
 * This plugin makes the app delegate replace the shared cache with one that
 * has no memory and no disk capacity, first thing at launch, before React
 * Native creates a session. It also empties the old shared cache, so a file
 * left by an earlier build is emptied on the first launch of this one.
 * `endSession` additionally deletes the caches directory (src/security/cacheWipe.ts).
 *
 * Android (SEC-1): React Native's networking module and `expo/fetch` build
 * their OkHttp client with `OkHttpClientProvider.createClient(context)`, which
 * attaches a 10 MB plaintext disk cache at `cacheDir/http-cache`. OkHttp
 * stores any cacheable 200 GET there, the SSE event streams (`no-cache`, not
 * `no-store`) included. The plugin makes `MainApplication.onCreate` install a
 * client factory built from the cache-less `createClientBuilder()`, before
 * React Native loads (the factory must be set before any networking module is
 * created), and delete an `http-cache` directory left by an earlier build.
 */

const TAG = 'agent-zefir-no-http-cache';
const BEGIN = `// @generated begin ${TAG}`;
const END = `// @generated end ${TAG}`;

const SNIPPET = [
  BEGIN,
  '// Responses are never written to a plaintext URL cache (DEF-R2, plugins/withNoHttpCache.js).',
  'URLCache.shared.removeAllCachedResponses()',
  'URLCache.shared = URLCache(memoryCapacity: 0, diskCapacity: 0, directory: nil)',
  END,
];

const LAUNCH = /func application\(\s*_ application: UIApplication,\s*didFinishLaunchingWithOptions[^{]*\{\n/;

/**
 * Returns the Swift app delegate with the no-cache lines as the first
 * statements of `application(_:didFinishLaunchingWithOptions:)`. Idempotent.
 * Throws when that method is not found: silently building an app that caches
 * responses in plaintext is worse than a failed prebuild.
 *
 * @param {string} contents
 * @returns {string}
 */
function addNoHttpCache(contents) {
  contents = removeBlock(contents);
  const match = LAUNCH.exec(contents);
  if (!match) {
    throw new Error(
      `${TAG}: application(_:didFinishLaunchingWithOptions:) not found in the Swift AppDelegate`,
    );
  }
  const at = match.index + match[0].length;
  const indent = '    ';
  const block = SNIPPET.map((line) => `${indent}${line}\n`).join('');
  return contents.slice(0, at) + block + contents.slice(at);
}

const ANDROID_SNIPPET = [
  BEGIN,
  '// Responses are never written to a plaintext HTTP disk cache (SEC-1, plugins/withNoHttpCache.js).',
  'com.facebook.react.modules.network.OkHttpClientProvider.setOkHttpClientFactory {',
  '  com.facebook.react.modules.network.OkHttpClientProvider.createClientBuilder().build()',
  '}',
  'java.io.File(cacheDir, "http-cache").deleteRecursively()',
  END,
];

const ON_CREATE = /override fun onCreate\(\)\s*\{\n(\s*)super\.onCreate\(\)\n/;

/**
 * Returns the Kotlin MainApplication with the no-cache lines right after
 * `super.onCreate()`, before React Native loads. Idempotent. Throws when
 * `onCreate` is not found, for the same reason as the iOS half.
 *
 * @param {string} contents
 * @returns {string}
 */
function addNoHttpCacheAndroid(contents) {
  contents = removeBlock(contents);
  const match = ON_CREATE.exec(contents);
  if (!match) {
    throw new Error(`${TAG}: onCreate() with super.onCreate() not found in the Kotlin MainApplication`);
  }
  const at = match.index + match[0].length;
  const indent = match[1];
  const block = ANDROID_SNIPPET.map((line) => `${indent}${line}\n`).join('');
  return contents.slice(0, at) + block + contents.slice(at);
}

/**
 * @param {string} contents
 * @returns {string}
 */
function removeBlock(contents) {
  const begin = contents.indexOf(BEGIN);
  if (begin === -1) return contents;
  const end = contents.indexOf(END, begin);
  const lineStart = contents.lastIndexOf('\n', begin) + 1;
  const lineEnd = contents.indexOf('\n', end) + 1;
  return contents.slice(0, lineStart) + contents.slice(lineEnd);
}

/** @type {import('expo/config-plugins').ConfigPlugin} */
const withNoHttpCache = (config) => {
  config = withAppDelegate(config, (mod) => {
    if (mod.modResults.language !== 'swift') {
      throw new Error(`${TAG}: expected a Swift AppDelegate, got ${mod.modResults.language}`);
    }
    mod.modResults.contents = addNoHttpCache(mod.modResults.contents);
    return mod;
  });
  return withMainApplication(config, (mod) => {
    if (mod.modResults.language !== 'kt') {
      throw new Error(`${TAG}: expected a Kotlin MainApplication, got ${mod.modResults.language}`);
    }
    mod.modResults.contents = addNoHttpCacheAndroid(mod.modResults.contents);
    return mod;
  });
};

module.exports = withNoHttpCache;
module.exports.addNoHttpCache = addNoHttpCache;
module.exports.addNoHttpCacheAndroid = addNoHttpCacheAndroid;
module.exports.TAG = TAG;
