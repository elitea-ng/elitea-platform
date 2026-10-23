// @ts-check
const fs = require('node:fs');
const path = require('node:path');
const { withXcodeProject } = require('expo/config-plugins');

/**
 * A privacy manifest (PrivacyInfo.xcprivacy) for an iOS app EXTENSION target.
 *
 * Expo's `ios.privacyManifests` writes the app target's manifest only. Apple's
 * upload scan checks every binary in the bundle, extensions included
 * (ITMS-91053 names `PlugIns/<name>.appex/<name>`), and the widget extension
 * links React Native and expo-widgets statically: their UserDefaults (the App
 * Group suite), file-timestamp and boot-time calls need declaring there too.
 * This plugin writes `ios/<target>/PrivacyInfo.xcprivacy` and adds it to that
 * target's Resources phase.
 *
 * List it BEFORE the plugin that creates the target (expo-widgets): Xcode
 * project mods run last-registered first, so it then sees the target.
 * A missing target fails prebuild: an upload rejected weeks later is worse.
 *
 * Options: { targetName: string, accessedAPITypes: { type: string, reasons: string[] }[] }
 */

const FILE = 'PrivacyInfo.xcprivacy';

/** @param {string} text */
function xmlEscape(text) {
  return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

/**
 * The manifest's XML: no tracking, no collected data (the extension sends
 * nothing anywhere), and the given required-reason APIs.
 *
 * @param {{ type: string, reasons: string[] }[]} accessedAPITypes
 * @returns {string}
 */
function privacyManifestXml(accessedAPITypes) {
  const entries = accessedAPITypes
    .map(
      ({ type, reasons }) =>
        [
          '\t\t<dict>',
          '\t\t\t<key>NSPrivacyAccessedAPIType</key>',
          `\t\t\t<string>${xmlEscape(type)}</string>`,
          '\t\t\t<key>NSPrivacyAccessedAPITypeReasons</key>',
          '\t\t\t<array>',
          ...reasons.map((reason) => `\t\t\t\t<string>${xmlEscape(reason)}</string>`),
          '\t\t\t</array>',
          '\t\t</dict>',
        ].join('\n'),
    )
    .join('\n');
  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">',
    '<plist version="1.0">',
    '<dict>',
    '\t<key>NSPrivacyAccessedAPITypes</key>',
    '\t<array>',
    entries,
    '\t</array>',
    '\t<key>NSPrivacyCollectedDataTypes</key>',
    '\t<array/>',
    '\t<key>NSPrivacyTracking</key>',
    '\t<false/>',
    '\t<key>NSPrivacyTrackingDomains</key>',
    '\t<array/>',
    '</dict>',
    '</plist>',
    '',
  ].join('\n');
}

/**
 * Adds `<targetName>/PrivacyInfo.xcprivacy` to the target's group and Resources
 * phase (creating the phase when the target has none). Idempotent.
 *
 * @param {any} project an `xcode` project (config.modResults)
 * @param {string} targetName
 */
function addManifestToTarget(project, targetName) {
  const targetKey = project.findTargetKey(targetName);
  if (!targetKey) {
    throw new Error(`withExtensionPrivacyManifest: no Xcode target "${targetName}" (list this plugin before the one that creates it)`);
  }
  const groupKey = project.findPBXGroupKey({ name: targetName }) ?? project.findPBXGroupKey({ path: targetName });
  if (!groupKey) {
    throw new Error(`withExtensionPrivacyManifest: no Xcode group "${targetName}"`);
  }
  const group = project.getPBXGroupByKey(groupKey);
  const fileRefs = project.pbxFileReferenceSection();
  const already = group.children.some((/** @type {{ value: string }} */ child) => {
    const ref = fileRefs[child.value];
    return ref && String(ref.path).replace(/"/g, '') === FILE;
  });
  if (already) return project;
  // Not addResourceFile / pbxResourcesBuildPhaseObj: the `xcode` package assumes a top-level
  // "Resources" group, and for a target without a Resources phase it returns ANOTHER
  // target's phase (the app's), which would ship the file in the wrong bundle.
  const phase = resourcesPhaseOf(project, targetKey) ?? addResourcesPhase(project, targetKey);
  const file = project.addFile(FILE, groupKey, { lastKnownFileType: 'text.xml' });
  if (!file) {
    throw new Error(`withExtensionPrivacyManifest: could not add ${FILE} to "${targetName}"`);
  }
  file.uuid = project.generateUuid();
  file.target = targetKey;
  project.addToPbxBuildFileSection(file);
  phase.files.push({ value: file.uuid, comment: `${FILE} in Resources` });
  return project;
}

/**
 * The target's own PBXResourcesBuildPhase, or null.
 *
 * @param {any} project
 * @param {string} targetKey
 */
function resourcesPhaseOf(project, targetKey) {
  const target = project.pbxNativeTargetSection()[targetKey];
  const phases = project.hash.project.objects.PBXResourcesBuildPhase ?? {};
  for (const { value } of target.buildPhases ?? []) {
    if (phases[value]) return phases[value];
  }
  return null;
}

/**
 * @param {any} project
 * @param {string} targetKey
 */
function addResourcesPhase(project, targetKey) {
  const { uuid } = project.addBuildPhase([], 'PBXResourcesBuildPhase', 'Resources', targetKey);
  return project.hash.project.objects.PBXResourcesBuildPhase[uuid];
}

/** @type {import('expo/config-plugins').ConfigPlugin<{ targetName: string, accessedAPITypes: { type: string, reasons: string[] }[] }>} */
const withExtensionPrivacyManifest = (config, { targetName, accessedAPITypes }) =>
  withXcodeProject(config, (cfg) => {
    const dir = path.join(cfg.modRequest.platformProjectRoot, targetName);
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, FILE), privacyManifestXml(accessedAPITypes));
    cfg.modResults = addManifestToTarget(cfg.modResults, targetName);
    return cfg;
  });

module.exports = withExtensionPrivacyManifest;
module.exports.privacyManifestXml = privacyManifestXml;
module.exports.addManifestToTarget = addManifestToTarget;
