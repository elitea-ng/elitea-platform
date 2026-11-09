// @ts-check
const fs = require('node:fs');
const path = require('node:path');
const { AndroidConfig, withAndroidManifest, withAppBuildGradle, withDangerousMod } = require('expo/config-plugins');

/**
 * Android managed configuration (ADR-0052 decision 7, W1b): the keys an EMM (Intune,
 * Workspace ONE, Google's Test DPC) may set for this app. Android shows them in the EMM
 * console from `res/xml/app_restrictions.xml`, which the manifest names with
 * `<meta-data android:name="android.content.APP_RESTRICTIONS">`.
 *
 * The same keys as iOS AppConfig (src/security/managedConfig.ts reads both through one
 * parser; scripts/managed-configuration-plugin.test.mjs keeps the two lists equal). Each
 * key only tightens the workspace's policy; an unset key changes nothing.
 * native-modules/ManagedRestrictions.kt reads the values.
 */

const TAG = 'managed-configuration';
const META_NAME = 'android.content.APP_RESTRICTIONS';
const RESOURCE = 'app_restrictions';

/**
 * The managed keys. `title` and `description` are what the EMM console shows an
 * administrator; they are not product copy and name no product.
 * @type {ReadonlyArray<{ key: string; type: 'string' | 'bool' | 'integer'; title: string; description: string }>}
 */
const RESTRICTIONS = Object.freeze([
  {
    key: 'server_url',
    type: 'string',
    title: 'Workspace address',
    description: 'The workspace the app connects to (https://…). Shown read only on the connect screen.',
  },
  {
    key: 'lock_server',
    type: 'bool',
    title: 'Only this workspace',
    description: 'With a workspace address: the app keeps only that workspace and signs out of any other.',
  },
  {
    key: 'require_device_lock',
    type: 'bool',
    title: 'Require the app lock',
    description: 'Ask for the device passcode or biometrics to open the app.',
  },
  {
    key: 'idle_lock_seconds',
    type: 'integer',
    title: 'Lock after idle (seconds)',
    description: 'Lock the app after this many seconds in the background. The shorter of this and the workspace value wins.',
  },
  {
    key: 'allow_screenshots',
    type: 'bool',
    title: 'Allow screenshots',
    description: 'Off blocks screenshots and screen recording of the app.',
  },
]);

/**
 * MDM feedback (W6.1): keyed app states, androidx.enterprise. Pinned: the inline module
 * (native-modules/ManagedRestrictions.kt) compiles in the app project, so the app's
 * build.gradle carries it. Bump deliberately (1.1.0 is the latest stable).
 */
const ENTERPRISE_FEEDBACK = 'androidx.enterprise:enterprise-feedback:1.1.0';

/** build.gradle with the feedback dependency in `dependencies { … }` (idempotent). */
function addFeedbackDependency(gradle) {
  if (gradle.includes(ENTERPRISE_FEEDBACK)) return gradle;
  const at = gradle.search(/^dependencies\s*\{/m);
  if (at === -1) throw new Error('withManagedConfiguration: no dependencies { } block in app/build.gradle');
  const open = gradle.indexOf('{', at) + 1;
  return `${gradle.slice(0, open)}\n    // @generated ${TAG}: MDM feedback (keyed app states)\n    implementation("${ENTERPRISE_FEEDBACK}")${gradle.slice(open)}`;
}

function escapeXml(text) {
  return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

/** The `app_restrictions.xml` resource. Titles are literal strings (no string resources). */
function restrictionsXml(restrictions = RESTRICTIONS) {
  const entries = restrictions.map(
    (r) =>
      `  <restriction\n    android:key="${r.key}"\n    android:restrictionType="${r.type}"\n    android:title="${escapeXml(r.title)}"\n    android:description="${escapeXml(r.description)}" />`,
  );
  return [
    '<?xml version="1.0" encoding="utf-8"?>',
    `<!-- @generated ${TAG} (plugins/withManagedConfiguration.js) -->`,
    '<restrictions xmlns:android="http://schemas.android.com/apk/res/android">',
    ...entries,
    '</restrictions>',
    '',
  ].join('\n');
}

/** The manifest with the APP_RESTRICTIONS meta-data on the application (idempotent). */
function addRestrictionsMetaData(manifest) {
  const app = AndroidConfig.Manifest.getMainApplicationOrThrow(manifest);
  AndroidConfig.Manifest.addMetaDataItemToMainApplication(app, META_NAME, `@xml/${RESOURCE}`, 'resource');
  return manifest;
}

/** @type {import('expo/config-plugins').ConfigPlugin} */
const withManagedConfiguration = (config) => {
  config = withAndroidManifest(config, (mod) => {
    mod.modResults = addRestrictionsMetaData(mod.modResults);
    return mod;
  });
  config = withAppBuildGradle(config, (mod) => {
    if (mod.modResults.language !== 'groovy') throw new Error('withManagedConfiguration: app/build.gradle is not Groovy');
    mod.modResults.contents = addFeedbackDependency(mod.modResults.contents);
    return mod;
  });
  return withDangerousMod(config, [
    'android',
    async (mod) => {
      const dir = path.join(mod.modRequest.platformProjectRoot, 'app', 'src', 'main', 'res', 'xml');
      await fs.promises.mkdir(dir, { recursive: true });
      await fs.promises.writeFile(path.join(dir, `${RESOURCE}.xml`), restrictionsXml());
      return mod;
    },
  ]);
};

module.exports = withManagedConfiguration;
module.exports.RESTRICTIONS = RESTRICTIONS;
module.exports.restrictionsXml = restrictionsXml;
module.exports.addRestrictionsMetaData = addRestrictionsMetaData;
module.exports.META_NAME = META_NAME;
module.exports.ENTERPRISE_FEEDBACK = ENTERPRISE_FEEDBACK;
module.exports.addFeedbackDependency = addFeedbackDependency;
