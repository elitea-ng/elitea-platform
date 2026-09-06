/**
 * pages/credentials/configurationReference.ts — the value shape a
 * `'configuration'` schema field carries, read and written in one place.
 *
 * A model configuration links to the credential it uses by NAME, in an
 * object: `"ai_credentials": {"elitea_title": "vllm_creds", "private":
 * false}`. Three independent readers depend on that object:
 *
 *  - the gateway's `modelCredentialRef`
 *    (services/elitea-llm-gateway/internal/llmproxy/models.go), which
 *    decodes `elitea_title`/`alita_title` plus `private`;
 *  - elitea-main's create normalizer
 *    (services/elitea-main/internal/application/configurations/
 *    local_configurations_normalizer.go), which refuses a value that is not
 *    an object;
 *  - the admin platform-model dialog (`pages/admin/PlatformModelDialog.tsx`),
 *    which has always written `{elitea_title}`.
 *
 * The project-side form wrote a BARE STRING, because the schema property fell
 * through to the free-text widget. This module is the shared reader/writer
 * that makes the project side agree with the other three.
 *
 * `alita_title` is the pre-debranding spelling of the same field. A database
 * that has not run the rename task still holds it, so a READ accepts it; a
 * WRITE always emits `elitea_title`.
 */

/** One stored link to another configuration row, as the wire carries it. */
export interface ConfigurationReference {
  readonly elitea_title: string;
  /** True selects the CALLER's personal-project row instead of the owner's. */
  readonly private: boolean;
}

function readTitle(record: Readonly<Record<string, unknown>>): string {
  const elitea = record['elitea_title'];
  if (typeof elitea === 'string' && elitea !== '') return elitea;
  const alita = record['alita_title'];
  return typeof alita === 'string' ? alita : '';
}

/**
 * Narrows a stored field value to a reference, or `null` when it names
 * nothing.
 *
 * A bare string is deliberately NOT accepted as a reference here: the form
 * must not present a value it cannot round-trip through the object shape
 * every reader wants. It is still rendered as the selected option — see
 * `configurationReferenceTitle`.
 */
export function toConfigurationReference(value: unknown): ConfigurationReference | null {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return null;
  const record = value as Readonly<Record<string, unknown>>;
  const title = readTitle(record);
  if (title === '') return null;
  return { elitea_title: title, private: record['private'] === true };
}

/**
 * The title to show as the picker's current selection.
 *
 * It accepts the bare-string form too, so a row already damaged by the
 * defect this module fixes still shows what it names — and re-saving it
 * through the picker repairs it into the object shape.
 */
export function configurationReferenceTitle(value: unknown): string {
  if (typeof value === 'string') return value;
  return toConfigurationReference(value)?.elitea_title ?? '';
}

/** The object a picked option is written back as. */
export function buildConfigurationReference(title: string, isPrivate: boolean): ConfigurationReference {
  return { elitea_title: title, private: isPrivate };
}
