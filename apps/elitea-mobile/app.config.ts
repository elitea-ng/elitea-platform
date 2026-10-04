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
  /** Schemes accepted for connect links, e.g. `zefir://connect?server=https://...`. First is primary. */
  connectSchemes: ['zefir', 'agentzefir'],
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
  icon: './assets/images/icon.png', // TODO: replace placeholder with the SaaS-default icon set.
  scheme: [...identity.connectSchemes, identity.oauthRedirectScheme],
  userInterfaceStyle: 'automatic',
  ios: {
    bundleIdentifier: identity.bundleId,
    supportsTablet: true,
    icon: './assets/expo.icon', // TODO: placeholder (template icon); see docs/design.md "Icons".
    infoPlist: {
      // TODO(export-compliance): placeholder only. SQLCipher (encrypted offline store) and
      // AES-GCM attachment encryption are planned; this value MUST be reviewed before the
      // first App Store submission. See docs/open-questions.md.
      ITSAppUsesNonExemptEncryption: false,
    },
  },
  android: {
    package: identity.bundleId,
    allowBackup: false,
    adaptiveIcon: {
      backgroundColor: '#E6F4FE',
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
        backgroundColor: '#208AEF',
        image: './assets/images/splash-icon.png',
        imageWidth: 76,
      },
    ],
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
