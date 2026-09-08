/**
 * The folder source, against the three implementations it has to agree with.
 *
 * The bucket and prefix rules are elitea-main's
 * (`internal/providerhost/material/artifact.go`); the display form is the one
 * the wiki id is built from, and it has a Go twin (`run.DisplayRepositoryFor`)
 * and a Python twin (`wiki_context.display_repository_for`). A case here that
 * disagrees with either of those is a browser that looks for a wiki under an
 * id nothing wrote — which reads, on screen, as a generation that produced
 * nothing.
 */
import { describe, expect, it } from 'vitest';

import { getArtifactSource, readArtifactSource } from './artifactSource';
import { filterManifestsByRepo } from './repoMatch';
import { getConfiguredRepoIdentity } from './toolkitSettings';

describe('readArtifactSource', () => {
  it('reads a bucket and a folder, under either spelling of the key', () => {
    for (const key of ['artifact_configuration', 'toolkit_configuration_artifact_configuration']) {
      const reading = readArtifactSource({ [key]: { bucket: 'docs', prefix: 'handbook' } });
      expect(reading).toEqual({ status: 'ok', source: { bucket: 'docs', prefix: 'handbook' } });
    }
  });

  it('accepts an empty folder as the whole bucket, however it is spelled', () => {
    // The facade's own rule: `prefix` may be absent, empty, or all slashes,
    // and each names the bucket root. A browser that refused any of them
    // would refuse a source the server accepts.
    for (const prefix of [undefined, null, '', '   ', '/', '///']) {
      expect(getArtifactSource({ artifact_configuration: { bucket: 'docs', prefix } })).toEqual({
        bucket: 'docs',
        prefix: '',
      });
    }
  });

  it('canonicalises the folder the way the server does', () => {
    // `strings.Trim(strings.TrimSpace(prefix), "/")`. Written as typed, read
    // back canonical, so `docs/handbook/` and `docs/handbook` are one folder.
    expect(getArtifactSource({ artifact_configuration: { bucket: 'DOCS', prefix: ' /handbook/ ' } })).toEqual({
      bucket: 'docs',
      prefix: 'handbook',
    });
  });

  it('ACCEPTS the folders and buckets the server accepts', () => {
    // Every case above says what is refused, and a rule that refuses too much
    // passes all of them. THIS is the case that caught one: the refused-
    // character class was written `[\\u0000]`, in which the `u`, the `0` and
    // the backslash are three separate members — so `documentation/v10` was
    // refused for holding a `u`. Each value here must be accepted.
    const accepted: readonly string[] = [
      'documentation/v10',
      'docs/ui',
      'user-guide/00-intro',
      'a.b/c-d/e_f',
      'x'.repeat(1024),
      'ф'.repeat(341),
    ];
    for (const prefix of accepted) {
      expect(readArtifactSource({ artifact_configuration: { bucket: 'docs', prefix } }).status).toBe('ok');
    }
    // The bucket rule's own boundaries: the shortest name, the longest, and
    // one holding each character class the pattern allows.
    for (const bucket of ['ab', 'a1', 'a-b-1', `a${'z'.repeat(62)}`]) {
      expect(readArtifactSource({ artifact_configuration: { bucket } }).status).toBe('ok');
    }
  });

  it('refuses the two characters an object key may not hold', () => {
    // A backslash and a NUL, and nothing else that looks like them: the
    // escape sequence for a NUL is four characters that are all legal in a
    // folder name of their own.
    for (const prefix of ['a\\b', `a${String.fromCharCode(0)}b`]) {
      expect(readArtifactSource({ artifact_configuration: { bucket: 'docs', prefix } }).status).toBe('invalid');
    }
    expect(readArtifactSource({ artifact_configuration: { bucket: 'docs', prefix: 'u0000' } }).status).toBe('ok');
  });

  it('says nothing about settings that name no folder', () => {
    for (const settings of [null, undefined, 'a string', {}, { repository: 'acme/x' }, { artifact_configuration: null }]) {
      expect(readArtifactSource(settings)).toEqual({ status: 'absent' });
      expect(getArtifactSource(settings)).toBeNull();
    }
  });

  it('names the BUCKET when the bucket is the part that is wrong', () => {
    // Each of these is refused by `^[a-z][a-z0-9-]{1,62}$` for a different
    // reason: an upper-case letter that survives lowering is impossible, so
    // the cases are the leading digit, the underscore, the single character,
    // the over-long name, and the absent one.
    for (const bucket of ['1docs', 'my_docs', 'd', 'a'.repeat(64), '', 42, null]) {
      const reading = readArtifactSource({ artifact_configuration: { bucket } });
      expect(reading.status).toBe('invalid');
      expect(reading.status === 'invalid' ? reading.field : '').toBe('artifact_configuration.bucket');
    }
  });

  it('names the FOLDER when the folder is the part that is wrong', () => {
    // elitea-main's object-key rules: no empty, `.` or `..` segment, no
    // backslash, at most 1024 bytes. `..` is the one that matters most — it
    // is how a prefix would try to leave the folder it names.
    for (const prefix of ['a//b', 'a/../b', './a', 'a/..', 'a\\b', 'x'.repeat(1025)]) {
      const reading = readArtifactSource({ artifact_configuration: { bucket: 'docs', prefix } });
      expect(reading.status).toBe('invalid');
      expect(reading.status === 'invalid' ? reading.field : '').toBe('artifact_configuration.prefix');
    }
  });

  it('measures the folder in BYTES, not in characters', () => {
    // The server's limit is `len(prefix)` over UTF-8 bytes. 600 three-byte
    // characters are 1800 bytes and 600 characters: a browser counting
    // characters would accept a folder the server refuses.
    const prefix = 'ф'.repeat(600);
    expect(prefix.length).toBeLessThan(1024);
    expect(readArtifactSource({ artifact_configuration: { bucket: 'docs', prefix } }).status).toBe('invalid');
  });

  it('refuses a folder that is not a string, and a block that is not an object', () => {
    const numericFolder = readArtifactSource({ artifact_configuration: { bucket: 'docs', prefix: 42 } });
    expect(numericFolder.status === 'invalid' ? numericFolder.field : '').toBe('artifact_configuration.prefix');
    for (const block of ['docs', 42, ['docs']]) {
      const reading = readArtifactSource({ artifact_configuration: block });
      expect(reading.status === 'invalid' ? reading.field : '').toBe('artifact_configuration');
    }
  });
});

describe('the identity a folder source resolves to', () => {
  it('is the DISPLAY form, not the artifact:// form', () => {
    // The scheme is what the facade derives for the provider. The wiki is
    // named after this form, and `artifact:----docs--handbook--main` is not
    // a name anybody asked for.
    expect(getConfiguredRepoIdentity(null, { artifact_configuration: { bucket: 'docs', prefix: 'handbook' } }, null))
      .toEqual({ repository: 'docs/handbook', branch: null });
    expect(getConfiguredRepoIdentity(null, { artifact_configuration: { bucket: 'docs', prefix: '' } }, null))
      .toEqual({ repository: 'docs', branch: null });
  });

  it('carries the branch the settings name, under every alias', () => {
    expect(
      getConfiguredRepoIdentity(
        null,
        { artifact_configuration: { bucket: 'docs' }, active_branch: 'release' },
        null,
      ),
    ).toEqual({ repository: 'docs', branch: 'release' });
  });

  it('wins over a repository stored beside it', () => {
    // The facade settles a body naming both on the FOLDER — it derives the
    // repository from the folder and overwrites whatever the body said. An
    // identity that preferred the repository would look for the wiki of a
    // generation that never ran.
    expect(
      getConfiguredRepoIdentity(
        null,
        { artifact_configuration: { bucket: 'docs' }, github_repository: 'acme/notes' },
        null,
      )?.repository,
    ).toBe('docs');
  });

  it('is not read from a hand-written artifact:// repository', () => {
    // The facade derives that string from the BLOCK; a repository holding it
    // with no block names no source the facade would accept. The identity is
    // whatever the legacy parser makes of it, and nothing here pretends
    // otherwise — the settings form refuses the document on other grounds.
    expect(getArtifactSource({ repository: 'artifact://docs/handbook' })).toBeNull();
  });

  it('matches the wiki id the two fixture runners write', () => {
    // wiki_id_for: `{display repository with / as --}--{branch}`. Both twins
    // build it from the display form, so these ids are the ones the manifests
    // are stored under.
    const identity = getConfiguredRepoIdentity(
      null,
      { artifact_configuration: { bucket: 'docs', prefix: 'handbook' }, branch: 'main' },
      null,
    );
    const manifests = [
      { wiki_id: 'docs--handbook--main', repository: 'artifact://docs/handbook', branch: 'main' },
      { wiki_id: 'acme--notes--main', repository: 'acme/notes', branch: 'main' },
    ];
    expect(filterManifestsByRepo(manifests, identity).map((m) => m.wiki_id)).toEqual([
      'docs--handbook--main',
    ]);
  });
});
