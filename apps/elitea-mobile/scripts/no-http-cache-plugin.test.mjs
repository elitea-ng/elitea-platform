// DEF-R2 (Agent Zefir E2E run 2): API responses, the native token response
// (access + refresh token) included, were written by the default URL sessions
// to the plaintext NSURLCache (Library/Caches/<bundle id>/Cache.db) and
// survived revoke and "Log out & wipe". plugins/withNoHttpCache.js turns the
// shared URL cache off at launch; these tests pin that it is registered and
// that the generated Swift does it before React Native starts.
// Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createRequire } from 'node:module';
import { dirname, resolve } from 'node:path';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const require = createRequire(import.meta.url);
const { addNoHttpCache, addNoHttpCacheAndroid, TAG } = require('../plugins/withNoHttpCache.js');
const npx = process.platform === 'win32' ? 'npx.cmd' : 'npx';

// The SDK 58 template AppDelegate, as `expo prebuild` writes it.
const TEMPLATE = `internal import Expo
import React
import ReactAppDependencyProvider

@main
class AppDelegate: ExpoAppDelegate, ExpoReactNativeFactoryProvider {
  var window: UIWindow?

  var reactNativeDelegate: ExpoReactNativeFactoryDelegate?
  var reactNativeFactory: RCTReactNativeFactory?

  public override func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil
  ) -> Bool {
    let delegate = ReactNativeDelegate()
    let factory = ExpoReactNativeFactory(delegate: delegate)
    delegate.dependencyProvider = RCTAppDependencyProvider()

    reactNativeDelegate = delegate
    reactNativeFactory = factory

    return super.application(application, didFinishLaunchingWithOptions: launchOptions)
  }
}
`;

const ZERO_CACHE = 'URLCache.shared = URLCache(memoryCapacity: 0, diskCapacity: 0, directory: nil)';

describe('no plaintext URL cache (DEF-R2)', () => {
  it('is registered in the app config', () => {
    const output = execFileSync(npx, ['expo', 'config', '--type', 'prebuild', '--json'], {
      cwd: root,
      env: process.env,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'inherit'],
    });
    const plugins = JSON.parse(output).plugins ?? [];
    const names = plugins.map((p) => (Array.isArray(p) ? p[0] : p));
    assert.ok(
      names.some((name) => typeof name === 'string' && name.includes('withNoHttpCache')),
      `plugins: ${JSON.stringify(names)}`,
    );
  });

  it('replaces the shared URL cache with a zero-capacity one before React Native starts', () => {
    const out = addNoHttpCache(TEMPLATE);
    const zero = out.indexOf(ZERO_CACHE);
    assert.ok(zero > 0, out);
    assert.ok(out.indexOf('URLCache.shared.removeAllCachedResponses()') < zero);
    assert.ok(zero > out.indexOf('didFinishLaunchingWithOptions launchOptions'));
    assert.ok(zero < out.indexOf('let delegate = ReactNativeDelegate()'));
    assert.ok(zero < out.indexOf('return super.application('));
  });

  it('is idempotent', () => {
    const once = addNoHttpCache(TEMPLATE);
    assert.equal(addNoHttpCache(once), once);
    assert.equal(once.split(TAG).length - 1, 2);
  });

  it('fails the prebuild instead of silently keeping the cache when the launch method is missing', () => {
    assert.throws(() => addNoHttpCache('class AppDelegate {}\n'), /didFinishLaunchingWithOptions/);
  });
});

// SEC-1: on Android, React Native's networking module and expo/fetch build
// their OkHttp client with OkHttpClientProvider.createClient(context), which
// attaches a 10 MB plaintext disk cache (cacheDir/http-cache).
// The SDK 58 template MainApplication.kt, as `expo prebuild` writes it.
const MAIN_APPLICATION = `package ca.zefir.agent

import android.app.Application
import android.content.res.Configuration

import com.facebook.react.PackageList
import com.facebook.react.ReactApplication
import com.facebook.react.ReactNativeApplicationEntryPoint.loadReactNative
import com.facebook.react.ReactPackage
import com.facebook.react.ReactHost
import com.facebook.react.common.ReleaseLevel
import com.facebook.react.defaults.DefaultNewArchitectureEntryPoint

import expo.modules.ApplicationLifecycleDispatcher
import expo.modules.ExpoReactHostFactory

class MainApplication : Application(), ReactApplication {

  override val reactHost: ReactHost by lazy {
    ExpoReactHostFactory.getDefaultReactHost(
      context = applicationContext,
      packageList =
        PackageList(this).packages.apply {
        }
    )
  }

  override fun onCreate() {
    super.onCreate()
    DefaultNewArchitectureEntryPoint.releaseLevel = try {
      ReleaseLevel.valueOf(BuildConfig.REACT_NATIVE_RELEASE_LEVEL.uppercase())
    } catch (e: IllegalArgumentException) {
      ReleaseLevel.STABLE
    }
    loadReactNative(this)
    ApplicationLifecycleDispatcher.onApplicationCreate(this)
  }
}
`;

describe('no plaintext HTTP disk cache on Android (SEC-1)', () => {
  it('installs a cache-less OkHttp factory and deletes the old cache before React Native loads', () => {
    const out = addNoHttpCacheAndroid(MAIN_APPLICATION);
    const factory = out.indexOf('OkHttpClientProvider.setOkHttpClientFactory');
    assert.ok(factory > 0, out);
    assert.ok(out.includes('OkHttpClientProvider.createClientBuilder().build()'), out);
    assert.ok(!/createClientBuilder\((this|applicationContext|context)/.test(out), out);
    const wipe = out.indexOf('"http-cache").deleteRecursively()');
    assert.ok(wipe > 0, out);
    assert.ok(factory > out.indexOf('super.onCreate()'));
    assert.ok(factory < out.indexOf('loadReactNative(this)'));
    assert.ok(wipe < out.indexOf('loadReactNative(this)'));
  });

  it('is idempotent', () => {
    const once = addNoHttpCacheAndroid(MAIN_APPLICATION);
    assert.equal(addNoHttpCacheAndroid(once), once);
    assert.equal(once.split(TAG).length - 1, 2);
  });

  it('fails the prebuild instead of silently keeping the cache when onCreate is missing', () => {
    assert.throws(() => addNoHttpCacheAndroid('class MainApplication : Application()\n'), /onCreate/);
  });
});
