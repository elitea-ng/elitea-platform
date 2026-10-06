import type { ConfigContext, ExpoConfig } from 'expo/config';

/**
 * Single source of truth for Agent Zefir's fixed identifiers.
 *
 * Nothing else in the codebase may hardcode these values: runtime code reads
 * them through `src/config/identity.ts`, which reads `expoConfig.extra.identity`.
 *
 * `bundleId` is PERMANENT once published to the App Store / Play Store.
 */
const identity = {
  displayName: 'Agent Zefir',
  slug: 'agent-zefir',
  bundleId: 'ca.zefir.agent',
  /** Schemes accepted for connect links, e.g. `agentzefir://connect?server=https://...`. */
  connectSchemes: ['agentzefir'],
  /** RFC 8252 private-use (reverse-DNS) scheme for the OAuth redirect. */
  oauthRedirectScheme: 'ca.zefir.agent',
  oauthRedirectPath: '/oauth/callback',
  webUrl: 'https://agent.zefir.ca',
  privacyUrl: 'https://agent.zefir.ca/privacy',
  supportUrl: 'https://agent.zefir.ca/support',
} as const;

export type AppIdentity = typeof identity;

export default ({ config }: ConfigContext): ExpoConfig => ({
  ...config,
  name: identity.displayName,
  slug: identity.slug,
  version: '0.1.0',
  orientation: 'portrait',
  // App icons and splash: Agent Zefirca head mark (docs/assets.md). The alternate workspace
  // icon set ships with its config plugin (follow-up F1 in docs/design-system.md).
  icon: './assets/images/icon.png',
  scheme: [...identity.connectSchemes, identity.oauthRedirectScheme],
  userInterfaceStyle: 'automatic',
  ios: {
    bundleIdentifier: identity.bundleId,
    supportsTablet: true,
    // Icon Composer document: light and dark gradient fills; the system derives Dark, Tinted, Clear.
    icon: './assets/zefirca.icon',
    infoPlist: {
      // iOS names the app by CFBundleName in system prompts, e.g. the sign-in sheet's
      // '"<name>" Wants to Use "<host>" to Sign In'. Prebuild leaves it as $(PRODUCT_NAME),
      // the Xcode target name without the space ("AgentZefir"). Must stay under 16 characters.
      CFBundleName: identity.displayName,
      // TODO(export-compliance): placeholder only. The offline store uses SQLCipher (expo-sqlite
      // useSQLCipher) and AES-GCM attachment encryption is planned; this value MUST be reviewed
      // before the first App Store submission. See docs/open-questions.md.
      ITSAppUsesNonExemptEncryption: false,
    },
  },
  android: {
    package: identity.bundleId,
    allowBackup: false,
    adaptiveIcon: {
      backgroundColor: '#EDE4FA',
      foregroundImage: './assets/images/android-icon-foreground.png',
      backgroundImage: './assets/images/android-icon-background.png',
      monochromeImage: './assets/images/android-icon-monochrome.png',
    },
    predictiveBackGestureEnabled: false,
  },
  web: {
    output: 'static',
    favicon: './assets/images/favicon.png',
  },
  plugins: [
    'expo-router',
    [
      'expo-splash-screen',
      {
        // The `background` tokens (src/design/tokens.ts), so the splash hands over without a flash.
        backgroundColor: '#FBF7F9',
        image: './assets/images/splash-mascot.png',
        imageWidth: 200,
        resizeMode: 'contain',
        dark: { backgroundColor: '#141118', image: './assets/images/splash-mascot.png' },
      },
    ],
    // iOS crashes on Face ID use without NSFaceIDUsageDescription.
    [
      'expo-local-authentication',
      { faceIDPermission: `${identity.displayName} uses Face ID to unlock your workspace.` },
    ],
    // The offline store is SQLCipher (src/db/). Needs a native build; Expo Go and web have
    // no SQLCipher, and the store refuses to run without it (src/db/open.ts).
    ['expo-sqlite', { useSQLCipher: true }],
    // Attachments: the photo library and the camera, asked for only when the user picks.
    // No microphone (photos only), so RECORD_AUDIO is blocked on Android. On Android 13+
    // the system photo picker needs no media permission at all.
    [
      'expo-image-picker',
      {
        photosPermission: `${identity.displayName} attaches the photos you choose to a message.`,
        cameraPermission: `${identity.displayName} uses the camera to take a photo you attach to a message.`,
        microphonePermission: false,
      },
    ],
    // No plaintext URL cache: responses (tokens, transcripts) are never written to
    // Library/Caches/<bundle id>/Cache.db (DEF-R2). Our own plugin, see plugins/.
    './plugins/withNoHttpCache',
  ],
  experiments: {
    typedRoutes: true,
    reactCompiler: true,
  },
  extra: {
    ...config.extra,
    identity,
  },
});
