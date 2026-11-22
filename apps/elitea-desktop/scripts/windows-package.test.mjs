// node --test scripts/windows-package.test.mjs (npm run test:scripts)
//
// The Windows packaging inputs (ADR-0052 W2b.2): the signing overlay for each signer,
// the MSIX version rule, and an MSIX manifest whose Publisher is the certificate subject
// (signed) or carries the unsigned-publisher OID (unsigned, until a signer exists).
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { appxManifest, msixPublisher, msixVersion, TIMESTAMP_URL, UNSIGNED_PUBLISHER_OID, windowsOverlay } from './windows-package.mjs';

test('an unsigned MSIX carries the unsigned-publisher OID; a signed one the certificate subject', () => {
  assert.equal(UNSIGNED_PUBLISHER_OID, 'OID.2.25.311729368913984317654407730594956997722=1');
  assert.equal(msixPublisher('EPAM Systems', ''), `CN=EPAM Systems, ${UNSIGNED_PUBLISHER_OID}`);
  assert.equal(msixPublisher('EPAM Systems', undefined), `CN=EPAM Systems, ${UNSIGNED_PUBLISHER_OID}`);
  // A distinguished-name special character is quoted.
  assert.equal(msixPublisher('Acme, Inc.', ' '), `CN="Acme, Inc.", ${UNSIGNED_PUBLISHER_OID}`);
  assert.equal(msixPublisher('Acme\\Corp "X"'), `CN="Acme\\\\Corp \\"X\\"", ${UNSIGNED_PUBLISHER_OID}`);
  assert.equal(msixPublisher('EPAM Systems', ' CN=EPAM Systems, O=EPAM Systems, C=US '), 'CN=EPAM Systems, O=EPAM Systems, C=US');
  assert.throws(() => msixPublisher('', ''), /no publisher/);
  const xml = appxManifest({ identifier: 'ai.elitea.desktop', displayName: 'Elitea', publisherDisplayName: 'EPAM Systems', publisher: msixPublisher('EPAM Systems'), version: '7.8.0', executable: 'elitea-desktop.exe' });
  assert.match(xml, /Publisher="CN=EPAM Systems, OID\.2\.25\.311729368913984317654407730594956997722=1"/);
});

const THUMB = 'ab'.repeat(20);

test('pfx signs by the imported certificate thumbprint, never a password', () => {
  const w = windowsOverlay('pfx', { WINDOWS_CERT_THUMBPRINT: THUMB }).bundle.windows;
  assert.equal(w.certificateThumbprint, THUMB.toUpperCase());
  assert.equal(w.digestAlgorithm, 'sha256');
  assert.equal(w.timestampUrl, TIMESTAMP_URL);
  assert.equal(w.signCommand, undefined);
  assert.throws(() => windowsOverlay('pfx', {}), /WINDOWS_CERT_THUMBPRINT is not set/);
  assert.throws(() => windowsOverlay('pfx', { WINDOWS_CERT_THUMBPRINT: 'nope' }), /not a SHA-1/);
});

test('trusted signs through artifact-signing-cli with %1 for each file', () => {
  const env = { AZURE_SIGNING_ENDPOINT: 'https://weu.codesigning.azure.net', AZURE_SIGNING_ACCOUNT: 'acct', AZURE_SIGNING_PROFILE: 'prof' };
  const { signCommand, certificateThumbprint } = windowsOverlay('trusted', env).bundle.windows;
  assert.equal(signCommand.cmd, 'artifact-signing-cli');
  assert.deepEqual(signCommand.args, ['-e', env.AZURE_SIGNING_ENDPOINT, '-a', 'acct', '-c', 'prof', '%1']);
  assert.equal(certificateThumbprint, undefined);
  assert.throws(() => windowsOverlay('trusted', { ...env, AZURE_SIGNING_PROFILE: ' ' }), /AZURE_SIGNING_PROFILE/);
});

test('none signs nothing; an unknown signer fails', () => {
  const w = windowsOverlay('none', {}).bundle.windows;
  assert.deepEqual(Object.keys(w), ['webviewInstallMode']);
  assert.equal(w.webviewInstallMode.silent, true);
  assert.throws(() => windowsOverlay('signtool', {}), /unknown signer/);
});

test('the MSIX version is X.Y.Z.0', () => {
  assert.equal(msixVersion('1.62.3'), '1.62.3.0');
  assert.throws(() => msixVersion('1.62.3-rc.1'), /not X.Y.Z/);
  assert.throws(() => msixVersion('1.70000.0'), /above 65535/);
});

test('the MSIX manifest names the brand, the certificate subject and the full-trust host', () => {
  const xml = appxManifest({
    identifier: 'ai.elitea.desktop',
    displayName: 'Elitea',
    publisherDisplayName: 'EPAM Systems',
    publisher: 'CN=EPAM Systems, O=EPAM Systems, C=US',
    version: '1.62.3',
    executable: 'elitea-desktop.exe',
  });
  assert.match(xml, /<Identity Name="ai\.elitea\.desktop" Publisher="CN=EPAM Systems, O=EPAM Systems, C=US" Version="1\.62\.3\.0"/);
  assert.match(xml, /<Application Id="App" Executable="elitea-desktop\.exe" EntryPoint="Windows\.FullTrustApplication">/);
  assert.match(xml, /<rescap:Capability Name="runFullTrust" \/>/);
  const escaped = appxManifest({ identifier: 'a.b.c', displayName: 'A & B <x>', publisherDisplayName: 'P', publisher: 'CN="Q"', version: '1.0.0', executable: 'a.exe' });
  assert.match(escaped, /A &amp; B &lt;x&gt;/);
  assert.match(escaped, /Publisher="CN=&quot;Q&quot;"/);
  assert.throws(() => appxManifest({ identifier: 'ai.elitea.desktop', publisher: 'EPAM', version: '1.0.0' }), /certificate subject/);
  assert.throws(() => appxManifest({ identifier: 'a b', publisher: 'CN=x', version: '1.0.0' }), /not a package name/);
});
