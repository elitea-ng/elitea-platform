// Test fixtures shared by scripts/brand/schema.test.mjs and scripts/brand-check.test.mjs.

/** A valid manifest; each test breaks one thing in a copy. */
export function validManifest(over = {}) {
  return {
    formatVersion: 1,
    id: 'acme',
    productName: 'Acme Assistant',
    shortName: 'Acme',
    publisher: 'Acme Corp',
    mobile: {
      bundleId: 'com.example.acme',
      connectSchemes: ['acme'],
      oauthRedirectScheme: 'com.example.acme',
      appGroup: 'group.com.example.acme',
      widgetBundleId: 'com.example.acme.widgets',
    },
    desktop: { identifier: 'com.example.acme.desktop' },
    linkDomain: 'acme.example.com',
    webUrl: 'https://acme.example.com',
    privacyUrl: 'https://acme.example.com/privacy',
    supportUrl: 'https://acme.example.com/support',
    supportEmail: 'support@acme.example.com',
    docsUrl: '/docs/',
    brand: { hue: '#0e7490' },
    eas: { projectId: null, owner: null },
    updates: { mode: 'off' },
    push: { relay: 'none' },
    ...over,
  };
}
