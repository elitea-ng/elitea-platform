/**
 * Reading an ARTIFACT FOLDER source out of a toolkit's stored settings.
 *
 * A toolkit may name a folder in the invoking project's artifact store —
 * `artifact_configuration: {bucket, prefix}` — INSTEAD of a `code_toolkit`.
 * The facade validates that block, derives `repository: artifact://...` from
 * it, and refuses a body that names both sources
 * (`services/elitea-main/internal/providerhost/material/artifact.go`).
 *
 * THE RULES BELOW ARE THAT FILE'S RULES, transcribed. A browser that accepts
 * a bucket the facade refuses accepts nothing: it only moves the refusal to a
 * 400 the operator reads as "the generation failed".
 *
 * ONLY `bucket` AND `prefix` ARE READ. The subapp host also accepts
 * `bucket_name` and `folder`, because it reads a payload the facade has
 * already rewritten. These settings are the facade's INPUT, and its input is
 * the two JSON tags on `ArtifactSourceIn`.
 */

/** The keys a folder source arrives under, prefixed or not. */
const ARTIFACT_SOURCE_KEYS = [
  'artifact_configuration',
  'toolkit_configuration_artifact_configuration',
] as const;

/** elitea-main's bucket rule (internal/api/v2/artifacts/handler.go). */
const BUCKET_NAME = /^[a-z][a-z0-9-]{1,62}$/;

/** internal/infra/storage/ref.go's object-key limit, counted in BYTES. */
const MAX_PREFIX_BYTES = 1024;

/**
 * The two characters an object key may never hold.
 *
 * NOT a character class. It was written as one, and the class
 * `[\\u0000]` holds FIVE members and not two: a backslash, a `u`
 * and three zeros. Every folder holding a `u` — `documentation`, `docs/ui` —
 * was refused by a rule that reads as if it refuses a NUL.
 */
const REFUSED_IN_KEY = ['\\', String.fromCharCode(0)];

/** One folder: a bucket in the invoking project, and a prefix that may be empty. */
interface ArtifactFolderSource {
  readonly bucket: string;
  readonly prefix: string;
}

/**
 * What the settings say about a folder source.
 *
 * The three states are distinct and each means something different. `absent`
 * is every repository generation. `invalid` is a source this deployment
 * refuses, and it names the field so a form can attach the message to it — an
 * operator told only "invalid" is left where the facade's 400 left them.
 */
type ArtifactSourceReading =
  | { readonly status: 'absent' }
  | { readonly status: 'ok'; readonly source: ArtifactFolderSource }
  | { readonly status: 'invalid'; readonly field: string; readonly message: string };

/** The prefix as an object-key prefix: trimmed, and without its outer slashes. */
function canonicalPrefix(value: string): string {
  return value.trim().replace(/^\/+|\/+$/g, '');
}

/**
 * elitea-main's object-key rules: no empty, `.` or `..` segment, at most 1024
 * bytes, and no backslash. The limit counts BYTES, so a folder named in a
 * non-Latin script is measured the way the server measures it.
 */
function prefixIsSafe(prefix: string): boolean {
  if (prefix === '') return true;
  if (new TextEncoder().encode(prefix).length > MAX_PREFIX_BYTES) return false;
  if (REFUSED_IN_KEY.some((character) => prefix.includes(character))) return false;
  return prefix.split('/').every((segment) => segment !== '' && segment !== '.' && segment !== '..');
}

/** One `artifact_configuration` block, read under the key it arrived under. */
function readArtifactBlock(block: Record<string, unknown>, key: string): ArtifactSourceReading {
  const rawBucket = block['bucket'];
  const bucket = typeof rawBucket === 'string' ? rawBucket.trim().toLowerCase() : '';
  if (!BUCKET_NAME.test(bucket)) {
    return {
      status: 'invalid',
      field: `${key}.bucket`,
      message:
        'A bucket name is 2 to 63 characters of lower-case letters, digits and hyphens, ' +
        'and it starts with a letter.',
    };
  }
  const rawPrefix = block['prefix'];
  if (rawPrefix !== undefined && rawPrefix !== null && typeof rawPrefix !== 'string') {
    return { status: 'invalid', field: `${key}.prefix`, message: 'The folder must be a string.' };
  }
  const prefix = canonicalPrefix(typeof rawPrefix === 'string' ? rawPrefix : '');
  if (!prefixIsSafe(prefix)) {
    return {
      status: 'invalid',
      field: `${key}.prefix`,
      message:
        'The folder must be a path inside the bucket: no empty, "." or ".." segment, ' +
        'no backslash, and at most 1024 bytes.',
    };
  }
  return { status: 'ok', source: { bucket, prefix } };
}

/**
 * The folder source the settings name, or why they name none.
 *
 * The first key that holds a block decides. A document carrying both
 * spellings carries one source under two names, so reading on after a
 * refusal would report the same folder twice.
 */
export function readArtifactSource(settings: unknown): ArtifactSourceReading {
  if (!settings || typeof settings !== 'object') return { status: 'absent' };
  const s = settings as Record<string, unknown>;
  for (const key of ARTIFACT_SOURCE_KEYS) {
    const block = s[key];
    if (block === undefined || block === null) continue;
    if (typeof block !== 'object' || Array.isArray(block)) {
      return {
        status: 'invalid',
        field: key,
        message: 'The artifact source must be an object naming a bucket and an optional folder.',
      };
    }
    return readArtifactBlock(block as Record<string, unknown>, key);
  }
  return { status: 'absent' };
}

/** The folder source the settings name, or null when they name none this deployment accepts. */
export function getArtifactSource(settings: unknown): ArtifactFolderSource | null {
  const reading = readArtifactSource(settings);
  return reading.status === 'ok' ? reading.source : null;
}

/**
 * A folder source as the wiki id and the manifest spell it: `bucket`, or
 * `bucket/prefix`.
 *
 * NOT the `artifact://` form. That form is what the facade derives for the
 * provider; the wiki is NAMED after this one. The Go and Python twins
 * (`run.DisplayRepositoryFor`, `wiki_context.display_repository_for`) strip
 * the scheme for the same reason: so that the id reads as a folder and not as
 * `artifact:----docs--handbook--main`. All three must agree, or this browser
 * looks for a wiki under an id nothing wrote.
 */
export function artifactDisplayRepository(source: ArtifactFolderSource): string {
  return source.prefix === '' ? source.bucket : `${source.bucket}/${source.prefix}`;
}

