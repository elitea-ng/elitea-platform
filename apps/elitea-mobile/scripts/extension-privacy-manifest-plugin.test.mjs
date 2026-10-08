// The widget extension ships its own PrivacyInfo.xcprivacy (Apple scans every
// binary in the bundle, ITMS-91053). plugins/withExtensionPrivacyManifest.js adds
// it to the EXTENSION target's Resources phase, never the app's: the `xcode`
// package hands back the app's phase for a target that has none of its own.
// Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { dirname } from 'node:path';
import { describe, it } from 'node:test';

const require = createRequire(import.meta.url);
// The `xcode` package the config plugins themselves use (a dependency of @expo/config-plugins).
const fromConfigPlugins = { paths: [dirname(require.resolve('@expo/config-plugins/package.json'))] };
const xcode = require(require.resolve('xcode', fromConfigPlugins));
const parser = require(require.resolve('xcode/lib/parser/pbxproj', fromConfigPlugins));
const { addManifestToTarget, privacyManifestXml } = require('../plugins/withExtensionPrivacyManifest.js');

// The shape prebuild writes: an app target with a Resources phase, an extension
// target (added by expo-widgets) with Sources and Frameworks only.
const PBXPROJ = `// !$*UTF8*$!
{
	archiveVersion = 1;
	classes = {
	};
	objectVersion = 46;
	objects = {
/* Begin PBXBuildFile section */
		B1 /* Expo.plist in Resources */ = {isa = PBXBuildFile; fileRef = F1 /* Expo.plist */; };
/* End PBXBuildFile section */
/* Begin PBXFileReference section */
		F1 /* Expo.plist */ = {isa = PBXFileReference; path = Expo.plist; sourceTree = "<group>"; };
		F2 /* index.swift */ = {isa = PBXFileReference; path = index.swift; sourceTree = "<group>"; };
/* End PBXFileReference section */
/* Begin PBXGroup section */
		G0 = {isa = PBXGroup; children = (G1 /* App */, G2 /* ExpoWidgetsTarget */, ); sourceTree = "<group>"; };
		G1 /* App */ = {isa = PBXGroup; children = (F1 /* Expo.plist */, ); name = App; path = App; sourceTree = "<group>"; };
		G2 /* ExpoWidgetsTarget */ = {isa = PBXGroup; children = (F2 /* index.swift */, ); name = ExpoWidgetsTarget; path = ExpoWidgetsTarget; sourceTree = "<group>"; };
/* End PBXGroup section */
/* Begin PBXNativeTarget section */
		T1 /* App */ = {isa = PBXNativeTarget; buildPhases = (R1 /* Resources */, ); name = App; productName = App; };
		T2 /* ExpoWidgetsTarget */ = {isa = PBXNativeTarget; buildPhases = (S2 /* Sources */, ); name = ExpoWidgetsTarget; productName = ExpoWidgetsTarget; };
/* End PBXNativeTarget section */
/* Begin PBXProject section */
		P0 /* Project object */ = {isa = PBXProject; mainGroup = G0; targets = (T1 /* App */, T2 /* ExpoWidgetsTarget */, ); };
/* End PBXProject section */
/* Begin PBXResourcesBuildPhase section */
		R1 /* Resources */ = {isa = PBXResourcesBuildPhase; buildActionMask = 2147483647; files = (B1 /* Expo.plist in Resources */, ); runOnlyForDeploymentPostprocessing = 0; };
/* End PBXResourcesBuildPhase section */
/* Begin PBXSourcesBuildPhase section */
		S2 /* Sources */ = {isa = PBXSourcesBuildPhase; buildActionMask = 2147483647; files = (); runOnlyForDeploymentPostprocessing = 0; };
/* End PBXSourcesBuildPhase section */
	};
	rootObject = P0 /* Project object */;
}
`;

function project() {
  const p = xcode.project('unused.pbxproj');
  p.hash = parser.parse(PBXPROJ);
  return p;
}

function resourcesOf(p, targetName) {
  const target = p.pbxNativeTargetSection()[p.findTargetKey(targetName)];
  const phases = p.hash.project.objects.PBXResourcesBuildPhase;
  const phase = target.buildPhases.map(({ value }) => phases[value]).find(Boolean);
  return phase ? phase.files.map((f) => f.comment) : null;
}

describe('withExtensionPrivacyManifest', () => {
  it("adds the manifest to the extension's own Resources phase, not the app's", () => {
    const p = addManifestToTarget(project(), 'ExpoWidgetsTarget');
    assert.deepEqual(resourcesOf(p, 'ExpoWidgetsTarget'), ['PrivacyInfo.xcprivacy in Resources']);
    assert.deepEqual(resourcesOf(p, 'App'), ['Expo.plist in Resources']);
    const group = p.getPBXGroupByKey(p.findPBXGroupKey({ name: 'ExpoWidgetsTarget' }));
    assert.ok(group.children.some((c) => c.comment === 'PrivacyInfo.xcprivacy'));
  });

  it('is idempotent', () => {
    const p = addManifestToTarget(addManifestToTarget(project(), 'ExpoWidgetsTarget'), 'ExpoWidgetsTarget');
    assert.deepEqual(resourcesOf(p, 'ExpoWidgetsTarget'), ['PrivacyInfo.xcprivacy in Resources']);
  });

  it('fails prebuild when the target does not exist (plugin listed after expo-widgets)', () => {
    assert.throws(() => addManifestToTarget(project(), 'Missing'), /no Xcode target "Missing"/);
  });

  it('declares the given APIs, no tracking and no collected data', () => {
    const xml = privacyManifestXml([{ type: 'NSPrivacyAccessedAPICategoryUserDefaults', reasons: ['CA92.1', '1C8F.1'] }]);
    assert.match(xml, /<string>NSPrivacyAccessedAPICategoryUserDefaults<\/string>/);
    assert.match(xml, /<string>1C8F\.1<\/string>/);
    assert.match(xml, /<key>NSPrivacyTracking<\/key>\s*<false\/>/);
    assert.match(xml, /<key>NSPrivacyCollectedDataTypes<\/key>\s*<array\/>/);
  });
});
