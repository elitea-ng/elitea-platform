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
  /**
   * App Group shared with the widget extension (widgets and Live Activities,
   * src/features/surfaces/). It carries COUNTS only unless the workspace
   * policy allows titles (`allow_system_surfaces`); never a credential.
   */
  appGroup: 'group.ca.zefir.agent',
  /** The widget extension's bundle id (home-screen and lock-screen widgets, Live Activities). */
  widgetBundleId: 'ca.zefir.agent.widgets',
} as const;

export type AppIdentity = typeof identity;

/** NSMicrophoneUsageDescription: the microphone is used only for on-device dictation. */
const MICROPHONE_USAGE = `${identity.displayName} uses the microphone only while you dictate a message. Speech is recognised on this device.`;

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
    // Android 16 (API 36) turns the predictive-back system animations on for apps that target
    // it; the app opts in (audit §3 S1). Back is handled through OnBackPressedDispatcher only:
    // react-native-screens pops stacks and dismisses sheets, RN Modal's onRequestClose closes
    // the LockAwareModal sheets. Edge-to-edge is always on (SDK 58 / API 36 cannot opt out):
    // every screen pads by the safe-area insets, the composer rides the IME insets
    // (react-native-keyboard-controller) and the navigation bar is transparent.
    predictiveBackGestureEnabled: true,
    softwareKeyboardLayoutMode: 'resize',
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
    // The picker takes photos only; the microphone text is the dictation one (the same
    // NSMicrophoneUsageDescription; `false` here would block RECORD_AUDIO for dictation too).
    // On Android 13+ the system photo picker needs no media permission at all.
    [
      'expo-image-picker',
      {
        photosPermission: `${identity.displayName} attaches the photos you choose to a message.`,
        cameraPermission: `${identity.displayName} uses the camera to take a photo you attach to a message.`,
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
            name: 'ZefirCounts',
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
    // extraction does not see intents inside a static pod).
    inlineModules: { watchedDirectories: ['app-intents'] },
  },
  extra: {
    ...config.extra,
    identity,
  },
});
