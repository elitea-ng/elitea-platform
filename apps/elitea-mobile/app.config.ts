import type { ConfigContext, ExpoConfig } from 'expo/config';

// The brand manifest loader (ADR-0052 decision 2). It is plain ESM with node's
// standard library only; Expo evaluates this file in node, which loads it with
// require(esm), and so does the jest setup (src/test/setupExpoConfig.ts).
// eslint-disable-next-line @typescript-eslint/no-require-imports
const { loadBrand, REPO_ROOT } = require('../../scripts/brand/load.mjs') as typeof import('../../scripts/brand/load.mjs');

/**
 * The selected brand: `ELITEA_BRAND` (a brand id under `brand/`, or a path to a
 * brand folder), unset meaning `brand/elitea`, EPAM's ELITEA. Read once per
 * evaluation of this file (each `expo config` run, each EAS build).
 */
const brand = loadBrand();
const manifest = brand.manifest;

/** A text file of the brand folder, trimmed (the loader reads; nothing here opens files). */
function readBrandText(rel: string): string {
  return brand.readText(rel).trim();
}

/**
 * A brand folder file as a path relative to this app (`../../brand/elitea/...`): the
 * config, and the native fingerprint EAS Update matches on, then hold no machine path.
 * Anchored on the repository root the loader reports, not the working directory.
 */
const APP_DIR = `${REPO_ROOT}/apps/elitea-mobile`;
const brandFile = (rel: string): string => brand.relativeTo(APP_DIR, rel);
const brandAsset = (rel: string): string => brandFile(`assets/${rel}`);

/** A brand folder file only when it exists (optional files: the Firebase pair). */
const optionalBrandFile = (rel: string): string | undefined => (brand.has(rel) ? brandFile(rel) : undefined);

/**
 * The scheme backgrounds behind the icon and splash: the brand's
 * `brand.schemes.<scheme>["background.default"]`, else Paper and Ink (the
 * same defaults brand-check measures the hue against).
 */
const background = {
  light: manifest.brand.schemes?.light?.['background.default'] ?? '#FFFFFF',
  dark: manifest.brand.schemes?.dark?.['background.default'] ?? '#0B0B18',
};

/**
 * The app's fixed identifiers, built from the brand manifest
 * (`brand/<id>/brand.json`, the only place they are written down).
 *
 * Runtime code reads them through `src/config/identity.ts`, which reads
 * `expoConfig.extra.identity`. Nothing else in the app may hardcode them.
 *
 * `bundleId` and `appGroup` are PERMANENT once a build of the brand is
 * uploaded to a store (`node scripts/brand-check.mjs` marks them).
 */
const identity = {
  displayName: manifest.productName,
  shortName: manifest.shortName,
  slug: `${manifest.id}-mobile`,
  bundleId: manifest.mobile.bundleId,
  /** Schemes accepted for connect links, e.g. `<scheme>://connect?server=https://...`. */
  connectSchemes: [...manifest.mobile.connectSchemes],
  /** RFC 8252 private-use (reverse-DNS) scheme for the OAuth redirect. */
  oauthRedirectScheme: manifest.mobile.oauthRedirectScheme,
  oauthRedirectPath: '/oauth/callback',
  /** Universal links and App Links host, or null: the custom scheme then works alone. */
  linkDomain: manifest.linkDomain,
  webUrl: manifest.webUrl,
  privacyUrl: manifest.privacyUrl,
  supportUrl: manifest.supportUrl,
  /**
   * App Group shared with the widget extension (widgets and Live Activities,
   * src/features/surfaces/). It carries COUNTS only unless the workspace
   * policy allows titles (`allow_system_surfaces`); never a credential.
   */
  appGroup: manifest.mobile.appGroup,
  /** The widget extension's bundle id (home-screen and lock-screen widgets, Live Activities). */
  widgetBundleId: manifest.mobile.widgetBundleId,
  /**
   * The brand's mark (`assets/logo/logo-mark.svg`) as markup, drawn by `<BrandMark>`: the
   * app's own identity on the welcome, lock, About and empty screens. brand-check holds it to
   * an SVG with a viewBox.
   */
  markSvg: readBrandText('assets/logo/logo-mark.svg'),
};

export type AppIdentity = typeof identity;

/** The brand id this config was built for (`extra.brand`); never used to branch behaviour. */
export const BRAND_ID = brand.id;

/** NSMicrophoneUsageDescription: the microphone is used only for on-device dictation. */
const MICROPHONE_USAGE = `${identity.displayName} uses the microphone only while you dictate a message. Speech is recognised on this device.`;

/**
 * The EAS project this app is linked to (docs/release.md, "Link the EAS project").
 *
 * Not a secret. Until the owner runs `eas init`, both values are empty and the
 * app is simply not linked: no `owner`, no `extra.eas.projectId`, and EAS Update
 * is OFF (`updates.enabled: false`, no `updates.url`). After `eas init`, write the
 * project id and owner here (recommended: every machine and CI then computes the
 * same runtime fingerprint), or set EAS_PROJECT_ID / EXPO_OWNER in the
 * environment, which win over the committed values.
 */
const easLink = {
  projectId: manifest.eas.projectId ?? '',
  owner: manifest.eas.owner ?? '',
};

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
/** Expo account or organization names: letters, digits, `-` and `_`. */
const EXPO_ACCOUNT = /^[a-z0-9][a-z0-9_-]*$/i;

export interface EasProjectLink {
  projectId?: string;
  owner?: string;
}

/** Resolves the EAS link from the environment, then the committed values; rejects malformed ones. */
export function resolveEasLink(env: Record<string, string | undefined> = process.env): EasProjectLink {
  const projectId = env.EAS_PROJECT_ID?.trim() || easLink.projectId;
  const owner = env.EXPO_OWNER?.trim() || easLink.owner;
  if (projectId && !UUID.test(projectId)) {
    throw new Error(`app.config.ts: EAS project id must be a UUID (from \`eas init\`), got "${projectId}"`);
  }
  if (owner && !EXPO_ACCOUNT.test(owner)) {
    throw new Error(`app.config.ts: Expo owner must be an Expo account or organization name, got "${owner}"`);
  }
  return { ...(projectId ? { projectId: projectId.toLowerCase() } : {}), ...(owner ? { owner } : {}) };
}

/**
 * EAS Update (over-the-air JS updates), docs/release.md "OTA updates".
 *
 * runtimeVersion `fingerprint`: an update reaches only binaries whose native layer
 * (native modules, config plugins, this config) hashes the same, so JS that needs a
 * native change can never be sent to an old binary. The build's channel (eas.json)
 * picks the branch. On by link only: an unlinked build has no update URL at all.
 * Downloads never block launch (fallbackToCacheTimeout 0): a new update applies on
 * the next cold start.
 */
function updatesConfig(link: EasProjectLink): ExpoConfig['updates'] {
  if (!link.projectId) return { enabled: false };
  return {
    enabled: true,
    url: `https://u.expo.dev/${link.projectId}`,
    checkAutomatically: 'ON_LOAD',
    fallbackToCacheTimeout: 0,
  };
}

/**
 * iOS privacy manifest (PrivacyInfo.xcprivacy) for the app target. Apple does not read
 * the manifests of static CocoaPods (every Expo module), so their required-reason APIs are
 * declared here, aggregated from node_modules/<pkg>/ios/PrivacyInfo.xcprivacy and from the
 * native sources of modules that ship none (docs/store/privacy.md, "Privacy manifest").
 * Collected data types match the App Store privacy label draft (same doc).
 */
const privacyManifests = (link: EasProjectLink): NonNullable<ExpoConfig['ios']>['privacyManifests'] => ({
  NSPrivacyTracking: false,
  NSPrivacyTrackingDomains: [],
  NSPrivacyAccessedAPITypes: [
    {
      // React Native, expo-constants, expo-system-ui, expo-updates, expo-eas-client (app-only
      // defaults: CA92.1); expo-widgets, expo-app-intents, expo-sharing (the App Group suite
      // shared with the widget extension: 1C8F.1).
      NSPrivacyAccessedAPIType: 'NSPrivacyAccessedAPICategoryUserDefaults',
      NSPrivacyAccessedAPITypeReasons: ['CA92.1', '1C8F.1'],
    },
    {
      // React Native, Folly, glog, boost, expo-application, SQLite (files in the container:
      // C617.1); expo-file-system (SDK wrapper: 0A2A.1; user-granted files: 3B52.1);
      // expo-document-picker reads the picked file's modification date (3B52.1).
      NSPrivacyAccessedAPIType: 'NSPrivacyAccessedAPICategoryFileTimestamp',
      NSPrivacyAccessedAPITypeReasons: ['C617.1', '0A2A.1', '3B52.1'],
    },
    {
      // React Native timing and boost: elapsed time between events in the app.
      NSPrivacyAccessedAPIType: 'NSPrivacyAccessedAPICategorySystemBootTime',
      NSPrivacyAccessedAPITypeReasons: ['35F9.1'],
    },
    {
      // expo-file-system free/total disk space (E174.1 write check, 85F4.1 shown to the user);
      // SQLite statfs for the file system's flags.
      NSPrivacyAccessedAPIType: 'NSPrivacyAccessedAPICategoryDiskSpace',
      NSPrivacyAccessedAPITypeReasons: ['E174.1', '85F4.1'],
    },
  ],
  NSPrivacyCollectedDataTypes: [
    // Owner decision (docs/store/privacy.md, "Who collects"): the conservative answer,
    // declared because the app sends these to the workspace server the user signs in to.
    ...(['NSPrivacyCollectedDataTypeUserID', 'NSPrivacyCollectedDataTypeOtherUserContent', 'NSPrivacyCollectedDataTypePhotosorVideos', 'NSPrivacyCollectedDataTypeSearchHistory'] as const).map(
      (type) => ({
        NSPrivacyCollectedDataType: type,
        NSPrivacyCollectedDataTypeLinked: true,
        NSPrivacyCollectedDataTypeTracking: false,
        NSPrivacyCollectedDataTypePurposes: ['NSPrivacyCollectedDataTypePurposeAppFunctionality'],
      }),
    ),
    // EAS Update, only when it is on (a linked build): a random per-installation id
    // (expo-eas-client) sent to Expo with each update check, not linked to the user.
    ...(link.projectId
      ? [
          {
            NSPrivacyCollectedDataType: 'NSPrivacyCollectedDataTypeDeviceID',
            NSPrivacyCollectedDataTypeLinked: false,
            NSPrivacyCollectedDataTypeTracking: false,
            NSPrivacyCollectedDataTypePurposes: ['NSPrivacyCollectedDataTypePurposeAppFunctionality'],
          },
        ]
      : []),
  ],
});

/** Read once per evaluation of this file (each `expo config` run, each EAS build). */
const eas = resolveEasLink();

export default ({ config }: ConfigContext): ExpoConfig => ({
  ...config,
  name: identity.displayName,
  slug: identity.slug,
  ...(eas.owner ? { owner: eas.owner } : {}),
  version: '0.1.0',
  // Phones (and Android) stay portrait. iPad turns every way and runs in Split View, Slide Over
  // and Stage Manager (issue #15): with `ios.requireFullScreen` false, prebuild writes all four
  // orientations to UISupportedInterfaceOrientations~ipad (App Store multitasking needs them)
  // and UIRequiresFullScreen false. The Chats tab is two panes at regular width
  // (src/features/chats/twoPane.ts). scripts/ipad-orientation.test.mjs pins the Info.plist.
  orientation: 'portrait',
  // App icons and splash come from the brand folder (brand/<id>/assets/, docs/assets.md).
  // One icon; no alternate icons.
  icon: brandAsset('ios/icon-1024.png'),
  scheme: [...identity.connectSchemes, identity.oauthRedirectScheme],
  userInterfaceStyle: 'automatic',
  ios: {
    bundleIdentifier: identity.bundleId,
    privacyManifests: privacyManifests(eas),
    supportsTablet: true,
    // Resizable on iPad (see `orientation` above); never set this true, it would lock iPad to portrait.
    requireFullScreen: false,
    // The brand's Icon Composer document when it has one (the system derives Dark, Tinted,
    // Clear), else its 1024 icon.
    icon: brand.has('assets/ios/app.icon/icon.json') ? brandAsset('ios/app.icon') : brandAsset('ios/icon-1024.png'),
    // Firebase (push) only when the brand folder carries it; with no firebase/, nothing is wired.
    ...(optionalBrandFile('firebase/GoogleService-Info.plist') ? { googleServicesFile: optionalBrandFile('firebase/GoogleService-Info.plist') } : {}),
    infoPlist: {
      // iOS names the app by CFBundleName in system prompts, e.g. the sign-in sheet's
      // '"<name>" Wants to Use "<host>" to Sign In'. Prebuild leaves it as $(PRODUCT_NAME),
      // the Xcode target name without the space ("EliteaAI"). Must stay under 16 characters.
      CFBundleName: identity.displayName,
      // Export compliance (docs/store/export-compliance.md): on iOS every cryptographic
      // operation runs in Apple's operating system. TLS is URLSession; the offline store's
      // SQLCipher is compiled with SQLCIPHER_CRYPTO_CC (CommonCrypto AES-256, HMAC, PBKDF2,
      // SecRandomCopyBytes); keys live in the Keychain; expo-crypto is digests and random bytes
      // only. That is Apple's "encryption limited to that within the Apple operating system":
      // no documentation, no French declaration. Re-decide if any non-Apple crypto library
      // (OpenSSL, libsodium, a JS cipher) ever lands in the iOS build. Owner-confirmed before
      // the first submission (release checklist).
      ITSAppUsesNonExemptEncryption: false,
    },
  },
  android: {
    package: identity.bundleId,
    allowBackup: false,
    adaptiveIcon: {
      backgroundColor: background.dark,
      foregroundImage: brandAsset('android/adaptive-foreground.png'),
      backgroundImage: brandAsset('android/adaptive-background.png'),
      monochromeImage: brandAsset('android/adaptive-monochrome.png'),
    },
    ...(optionalBrandFile('firebase/google-services.json') ? { googleServicesFile: optionalBrandFile('firebase/google-services.json') } : {}),
    // Android 16 (API 36) turns the predictive-back system animations on for apps that target
    // it; the app opts in (audit §3 S1). Back is handled through OnBackPressedDispatcher only:
    // react-native-screens pops stacks and dismisses sheets, RN Modal's onRequestClose closes
    // the LockAwareModal sheets. Edge-to-edge is always on (SDK 58 / API 36 cannot opt out):
    // every screen pads by the safe-area insets, the composer rides the IME insets
    // (react-native-keyboard-controller) and the navigation bar is transparent.
    predictiveBackGestureEnabled: true,
    softwareKeyboardLayoutMode: 'resize',
    // "Display over other apps" comes from the prebuild template's optional list; nothing in the
    // app draws over other apps, and the store listing shows every permission (docs/store/privacy.md).
    blockedPermissions: ['android.permission.SYSTEM_ALERT_WINDOW'],
  },
  web: {
    output: 'static',
    favicon: brandAsset('web/favicon-48.png'),
  },
  plugins: [
    'expo-router',
    [
      'expo-splash-screen',
      {
        // The brand's scheme backgrounds (ELITEA: Paper light, Ink dark, the src/design/tokens.ts
        // `background` tokens), so the splash hands over to the first screen without a flash.
        backgroundColor: background.light,
        image: brandAsset('splash/splash-icon.png'),
        imageWidth: 200,
        resizeMode: 'contain',
        dark: { backgroundColor: background.dark, image: brandAsset('splash/splash-icon.png') },
      },
    ],
    // iOS crashes on Face ID use without NSFaceIDUsageDescription.
    [
      'expo-local-authentication',
      { faceIDPermission: `${identity.displayName} uses Face ID to unlock your workspace.` },
    ],
    // The offline store is SQLCipher (src/db/). Needs a native build; Expo Go and web have
    // no SQLCipher, and the store refuses to run without it (src/db/open.ts). FTS5 (on by
    // default, stated so it is never turned off by accident) is the offline chat search; its
    // index is a table inside the same encrypted file (src/db/search.ts,
    // scripts/sqlite-fts5-build.test.mjs).
    ['expo-sqlite', { useSQLCipher: true, enableFTS: true }],
    // Attachments: the photo library and the camera, asked for only when the user picks.
    // The picker takes photos only; the microphone text is the dictation one (the same
    // NSMicrophoneUsageDescription; `false` here would block RECORD_AUDIO for dictation too).
    // On Android 13+ the system photo picker needs no media permission at all.
    [
      'expo-image-picker',
      {
        photosPermission: `${identity.displayName} attaches the photos you choose to a message.`,
        // Also the document scanner's (VisionKit asks with NSCameraUsageDescription too).
        cameraPermission: `${identity.displayName} uses the camera to take a photo or scan a document you attach to a message.`,
        microphonePermission: MICROPHONE_USAGE,
      },
    ],
    // Dictation into the composer, ON THE DEVICE ONLY (src/features/speech/dictation.ts asks
    // for requiresOnDeviceRecognition; Apple's server recogniser is never used). Both
    // permissions are asked for the first time the user taps the microphone.
    [
      'expo-speech-recognition',
      {
        microphonePermission: MICROPHONE_USAGE,
        speechRecognitionPermission: `${identity.displayName} turns your speech into text on this device while you dictate. Audio is not sent to Apple or to your workspace.`,
      },
    ],
    // No plaintext URL cache: responses (tokens, transcripts) are never written to
    // Library/Caches/<bundle id>/Cache.db (DEF-R2). Our own plugin, see plugins/.
    './plugins/withNoHttpCache',
    // The widget extension's own PrivacyInfo.xcprivacy (Apple scans the .appex too): it links
    // React Native, expo-modules-core, expo-widgets and @expo/ui statically, which read the App
    // Group UserDefaults suite, file timestamps and boot time. Listed BEFORE expo-widgets, which
    // creates the target (Xcode project mods run last-registered first).
    [
      './plugins/withExtensionPrivacyManifest',
      {
        targetName: 'ExpoWidgetsTarget',
        accessedAPITypes: [
          { type: 'NSPrivacyAccessedAPICategoryUserDefaults', reasons: ['CA92.1', '1C8F.1'] },
          { type: 'NSPrivacyAccessedAPICategoryFileTimestamp', reasons: ['C617.1'] },
          { type: 'NSPrivacyAccessedAPICategorySystemBootTime', reasons: ['35F9.1'] },
        ],
      },
    ],
    // Home-screen and lock-screen widgets (counts per active workspace) and the Live Activity
    // of a running answer: one WidgetKit extension, fed from the App Group after sync
    // (src/features/surfaces/). Local updates only: no push token is ever requested.
    [
      'expo-widgets',
      {
        bundleIdentifier: identity.widgetBundleId,
        groupIdentifier: identity.appGroup,
        enablePushNotifications: false,
        frequentUpdates: false,
        widgets: [
          {
            name: 'EliteaCounts',
            displayName: identity.displayName,
            description: 'Pending approvals, unread notifications and running answers in your workspace.',
            ios: {
              supportedFamilies: ['systemSmall', 'systemMedium', 'accessoryCircular', 'accessoryRectangular', 'accessoryInline'],
            },
          },
        ],
      },
    ],
    // Siri, Shortcuts and Spotlight: OPEN-APP intents only (app-intents/*.swift). They hand an
    // invocation to JS, which opens a screen behind the device lock; nothing is answered to Siri.
    'expo-app-intents',
  ],
  experiments: {
    typedRoutes: true,
    reactCompiler: true,
    // The App Intents Swift (app-intents/) is compiled into the app target (Apple's metadata
    // extraction does not see intents inside a static pod). native-modules/: the document
    // scanner (VisionKit camera, PDF, on-device text recognition) for attachments.
    inlineModules: { watchedDirectories: ['app-intents', 'native-modules'] },
  },
  // Binaries and OTA updates match by fingerprint; eas.json's build profiles set the channel.
  runtimeVersion: { policy: 'fingerprint' },
  updates: updatesConfig(eas),
  extra: {
    ...config.extra,
    identity,
    brand: brand.id,
    // Merged, not replaced: config plugins (expo-widgets) add extra.eas.build for the extension.
    ...(eas.projectId ? { eas: { ...(config.extra?.eas as object | undefined), projectId: eas.projectId } } : {}),
  },
});
