/**
 * pages/credentials/llmModelConnectionTest.ts — the LLM model form's rules for
 * the "Test connection" button (legacy issue 6793).
 *
 * An `llm_model` is not a credential: it NAMES one (`ai_credentials`). Its
 * test asks the model itself to answer, with the values in the form, saved or
 * not. Three rules follow, and each one differs from a credential's test:
 *
 *  1. It always sends the FORM, never the stored row. An llm_model carries no
 *     secret: the server resolves the referenced credential itself, so the
 *     form is complete, and the edited model name is the one to test. On the
 *     edit screen the body also names the saved row (`configuration_id`): a
 *     private credential reference resolves for the row's AUTHOR, as it does
 *     when the saved model runs, not for whoever clicks Test.
 *  2. It needs AI credentials and a model name before it can run. The button
 *     says which is missing.
 *  3. A result describes one combination of credentials and model name.
 *     Changing either clears it. (The llm_model form has no API protocol
 *     field yet; the per-model DIAL protocol adds it to this set.)
 */
import { t } from '@/shared/i18n';

export const LLM_MODEL_TYPE = 'llm_model';

/** The form fields whose change makes a shown result stale. */
const RESULT_FIELDS: ReadonlySet<string> = new Set(['name', 'ai_credentials']);

/** The body key that names the saved row the edit screen tests. The server reads it; it is not a form field. */
const CONFIGURATION_ID_KEY = 'configuration_id';

/** True when the test must send the form values even on the edit screen. */
export function testsFormValues(configType: string | undefined): boolean {
  return configType === LLM_MODEL_TYPE;
}

/**
 * The body of an llm_model test: the form values, plus the saved row's id on
 * the edit screen. `undefined` configId (the create screen) sends the form
 * alone; the caller then becomes the author the server resolves for.
 */
export function modelTestBody(data: Readonly<Record<string, unknown>>, configId: string | undefined): Record<string, unknown> {
  if (configId === undefined || configId === '') return { ...data };
  return { ...data, [CONFIGURATION_ID_KEY]: configId };
}

/** True when editing `fieldKey` makes the shown test result stale. */
export function clearsTestResult(configType: string | undefined, fieldKey: string): boolean {
  return configType === LLM_MODEL_TYPE && RESULT_FIELDS.has(fieldKey);
}

function hasCredentialReference(value: unknown): boolean {
  if (typeof value !== 'object' || value === null) return false;
  const title = (value as Record<string, unknown>)['elitea_title'];
  return typeof title === 'string' && title.trim() !== '';
}

/**
 * The fields the test still needs, as labels for the disabled button's
 * tooltip. Empty when the test can run.
 */
export function missingForConnectionTest(configType: string | undefined, data: Readonly<Record<string, unknown>>): string[] {
  if (configType !== LLM_MODEL_TYPE) return [];
  const missing: string[] = [];
  if (!hasCredentialReference(data['ai_credentials'])) missing.push(t('credentials.form.test.aiCredentials', 'AI credentials'));
  const name = data['name'];
  if (typeof name !== 'string' || name.trim() === '') missing.push(t('credentials.form.test.modelName', 'Model name'));
  return missing;
}

/** The tooltip of a disabled Test button, or `''` when nothing is missing. */
export function missingFieldsTooltip(missing: readonly string[]): string {
  if (missing.length === 0) return '';
  return t('credentials.form.test.missingFields', 'Set {{fields}} to test the connection', { fields: missing.join(t('credentials.form.test.and', ' and ')) });
}

/** "Connected in 1.2 s", measured from the click to the answer. */
export function connectedInMessage(durationMs: number): string {
  const seconds = (Math.max(durationMs, 0) / 1000).toFixed(1);
  return t('credentials.form.test.connectedIn', 'Connected in {{seconds}} s', { seconds });
}
