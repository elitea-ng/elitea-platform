/**
 * The sentence an index's failure reads as (#2042).
 *
 * A failed index carries the worker's own text in `metadata.error`. Current
 * workers already put a plain sentence around a gateway fault code, but an
 * index that failed before that (or on an older worker) holds the bare form
 * "…refused the run (model_gateway.forbidden); nothing further was embedded",
 * which is all the badge used to say ("Index processing error"). The code
 * stays in the text for support; the words around it are what the user reads.
 */
import { t } from '@/shared/i18n';

const MAX_ERROR_CHARS = 600;

/** `model_gateway.*` -> what it means for the person who started the run. Unknown codes are left as the worker wrote them. */
function gatewayCodeSentence(code: string): string | undefined {
  switch (code) {
    case 'model_gateway.forbidden':
      return t('features.toolkits.indexError.forbidden', 'this project is not allowed to use that model (access denied by the provider or blocked by the LLM governance policy); choose another model in the toolkit settings or ask an administrator to allow it');
    case 'model_gateway.unauthorized':
      return t('features.toolkits.indexError.unauthorized', "the model's credential was rejected; check the credential of the model in AI configuration");
    case 'model_gateway.budget_exhausted':
    case 'model_gateway.member_budget_exhausted':
    case 'model_gateway.project_budget_exhausted':
      return t('features.toolkits.indexError.budget', 'the budget for model calls is used up; ask an administrator to raise it');
    case 'model_gateway.rejected':
      return t('features.toolkits.indexError.rejected', 'the model does not exist for this project or did not accept the request; check the model chosen in the toolkit settings');
    default:
      return undefined;
  }
}

/** The bare form: `(model_gateway.x)` followed directly by `;`, which a current worker never writes (it writes `): <sentence>;`). */
const BARE_REFUSAL = /\((model_gateway\.[a-z_]+)\);/;

/**
 * `metadata.error` as the readable message, or `undefined` when the index
 * carries none. A text that already explains itself is returned as it is.
 */
export function describeIndexError(raw: unknown): string | undefined {
  if (typeof raw !== 'string') return undefined;
  const text = raw.trim();
  if (text === '') return undefined;
  const bare = BARE_REFUSAL.exec(text);
  const sentence = bare?.[1] === undefined ? undefined : gatewayCodeSentence(bare[1]);
  const readable = bare === null || sentence === undefined ? text : text.replace(BARE_REFUSAL, `(${bare[1] ?? ''}): ${sentence};`);
  return readable.length > MAX_ERROR_CHARS ? `${readable.slice(0, MAX_ERROR_CHARS)}…` : readable;
}
