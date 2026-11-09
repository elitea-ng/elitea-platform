#!/usr/bin/env node
// The Windows half of .github/workflows/desktop-package.yml (ADR-0052 decisions 4 and 8;
// W2b.2, #1699): the Tauri signing overlay the MSI is built with, and the MSIX manifest
// makeappx packs. The job is off until the host builds on Windows (#1828); this module is
// tested now so the job only has to be switched on.
//
//   node scripts/windows-package.mjs overlay <trusted|pfx|none>   write src-tauri/tauri.windows.json
//   node scripts/windows-package.mjs manifest <layout-dir> <X.Y.Z> write <layout-dir>/AppxManifest.xml
//
// overlay reads WINDOWS_CERT_THUMBPRINT (pfx: the imported certificate) or
// AZURE_SIGNING_ENDPOINT / _ACCOUNT / _PROFILE (trusted); manifest reads WINDOWS_PUBLISHER.
// The brand comes from ELITEA_BRAND (scripts/brand/load.mjs). Secrets are never printed.

import { writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { loadBrand } from '../../../scripts/brand/load.mjs';
import { TAURI_DIR } from './tauri-brand.mjs';

export const OVERLAY_FILE = join(TAURI_DIR, 'tauri.windows.json');
/** RFC 3161 timestamping: a signature stays valid after the certificate expires. */
export const TIMESTAMP_URL = 'http://timestamp.acs.microsoft.com';

/** The MSIX version: four numeric parts, the last 0 (the Store reserves it). */
export function msixVersion(version) {
  const m = /^(\d+)\.(\d+)\.(\d+)$/.exec(String(version).trim());
  if (!m) throw new Error(`MSIX version: "${version}" is not X.Y.Z`);
  const parts = m.slice(1, 4).map(Number);
  if (parts.some((p) => p > 65535)) throw new Error(`MSIX version: "${version}" has a part above 65535`);
  return `${parts.join('.')}.0`;
}

/**
 * The tauri.conf.json overlay that signs the host executable, the sidecar and the MSI
 * as Tauri builds them. pfx: the certificate imported into CurrentUser\My, found by its
 * thumbprint (no password in any file). trusted: Azure Artifact Signing through
 * artifact-signing-cli, which reads AZURE_CLIENT_ID / _SECRET / _TENANT_ID itself.
 */
export function windowsOverlay(signer, env = process.env) {
  const need = (name) => {
    const v = env[name]?.trim();
    if (!v) throw new Error(`windows overlay (${signer}): ${name} is not set`);
    return v;
  };
  const windows = { webviewInstallMode: { type: 'downloadBootstrapper', silent: true } };
  if (signer === 'pfx') {
    const thumbprint = need('WINDOWS_CERT_THUMBPRINT');
    if (!/^[0-9A-Fa-f]{40}$/.test(thumbprint)) throw new Error('windows overlay (pfx): WINDOWS_CERT_THUMBPRINT is not a SHA-1 thumbprint');
    Object.assign(windows, { certificateThumbprint: thumbprint.toUpperCase(), digestAlgorithm: 'sha256', timestampUrl: TIMESTAMP_URL });
  } else if (signer === 'trusted') {
    // Tauri replaces %1 with each file to sign.
    windows.signCommand = {
      cmd: 'artifact-signing-cli',
      args: ['-e', need('AZURE_SIGNING_ENDPOINT'), '-a', need('AZURE_SIGNING_ACCOUNT'), '-c', need('AZURE_SIGNING_PROFILE'), '%1'],
    };
  } else if (signer !== 'none') {
    throw new Error(`windows overlay: unknown signer "${signer}" (trusted, pfx or none)`);
  }
  return { bundle: { windows } };
}

const xml = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

/**
 * AppxManifest.xml for the MSIX. The Identity Name is the brand's desktop identifier
 * (the AUMID becomes `<PackageFamilyName>!App`, #1828); Publisher must equal the signing
 * certificate's subject or Windows refuses to install the package.
 */
export function appxManifest({ identifier, displayName, publisherDisplayName, publisher, version, executable }) {
  if (!/^[A-Za-z0-9.-]{3,50}$/.test(identifier)) throw new Error(`MSIX: identifier "${identifier}" is not a package name (3-50 of A-Z a-z 0-9 . -)`);
  if (!/^CN=/.test(publisher ?? '')) throw new Error('MSIX: the publisher must be the certificate subject (CN=…)');
  return `<?xml version="1.0" encoding="utf-8"?>
<Package xmlns="http://schemas.microsoft.com/appx/manifest/foundation/windows10"
         xmlns:uap="http://schemas.microsoft.com/appx/manifest/uap/windows10"
         xmlns:rescap="http://schemas.microsoft.com/appx/manifest/foundation/windows10/restrictedcapabilities"
         IgnorableNamespaces="uap rescap">
  <Identity Name="${xml(identifier)}" Publisher="${xml(publisher)}" Version="${msixVersion(version)}" ProcessorArchitecture="x64" />
  <Properties>
    <DisplayName>${xml(displayName)}</DisplayName>
    <PublisherDisplayName>${xml(publisherDisplayName)}</PublisherDisplayName>
    <Logo>Assets\\StoreLogo.png</Logo>
  </Properties>
  <Dependencies>
    <TargetDeviceFamily Name="Windows.Desktop" MinVersion="10.0.19041.0" MaxVersionTested="10.0.26100.0" />
  </Dependencies>
  <Resources>
    <Resource Language="en-us" />
  </Resources>
  <Applications>
    <Application Id="App" Executable="${xml(executable)}" EntryPoint="Windows.FullTrustApplication">
      <uap:VisualElements DisplayName="${xml(displayName)}" Description="${xml(displayName)}" BackgroundColor="transparent"
                          Square150x150Logo="Assets\\Square150x150Logo.png" Square44x44Logo="Assets\\Square44x44Logo.png" />
    </Application>
  </Applications>
  <Capabilities>
    <rescap:Capability Name="runFullTrust" />
  </Capabilities>
</Package>
`;
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const [command, ...args] = process.argv.slice(2);
  try {
    if (command === 'overlay') {
      writeFileSync(OVERLAY_FILE, `${JSON.stringify(windowsOverlay(args[0]), null, 2)}\n`);
      console.log(`tauri.windows.json: signer ${args[0]}`);
    } else if (command === 'manifest') {
      const [dir, version] = args;
      if (!dir || !version) throw new Error('usage: windows-package.mjs manifest <layout-dir> <X.Y.Z>');
      const m = loadBrand().manifest;
      writeFileSync(
        join(dir, 'AppxManifest.xml'),
        appxManifest({
          identifier: m.desktop.identifier,
          displayName: m.shortName,
          publisherDisplayName: m.publisher,
          publisher: process.env.WINDOWS_PUBLISHER?.trim() || `CN=${m.publisher}`,
          version,
          executable: 'elitea-desktop.exe',
        }),
      );
      console.log(`AppxManifest.xml: ${m.desktop.identifier} ${msixVersion(version)}`);
    } else {
      throw new Error('usage: windows-package.mjs <overlay|manifest> …');
    }
  } catch (error) {
    console.log(`::error::${error.message}`);
    process.exitCode = 1;
  }
}
