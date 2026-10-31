/**
 * The guardrail part of the LLM Governance draft (ADR-0043 decision 1): the
 * fields, the detectors check and the draft <-> `data.guardrail` mapping.
 *
 * Kept out of `useGatewayGovernancePage.ts` so that file stays in its length
 * budget.
 */
import { t } from '@/shared/i18n';

/** The guardrail fields of a governance draft. Every number is a string, as in the rest of the draft. */
export interface GuardrailDraft {
  readonly guardrailStage: string;
  /** The ordered detectors as JSON text. The server checks each detector. */
  readonly guardrailDetectors: string;
  readonly guardrailTimeoutMs: string;
  /** "" keeps the default: closed when a detector blocks or masks, open when all flag. */
  readonly guardrailFailMode: string;
  readonly guardrailApplyToEmbeddings: boolean;
  readonly guardrailStreamWindow: string;
}

/** The detectors a new guardrail starts with: one secrets detector that masks. */
const DEFAULT_GUARDRAIL_DETECTORS = JSON.stringify([{ type: 'secrets', action: 'mask' }], null, 2);

export const EMPTY_GUARDRAIL_DRAFT: GuardrailDraft = {
  guardrailStage: 'input',
  guardrailDetectors: DEFAULT_GUARDRAIL_DETECTORS,
  guardrailTimeoutMs: '',
  guardrailFailMode: '',
  guardrailApplyToEmbeddings: false,
  guardrailStreamWindow: '',
};

/**
 * Parses the detectors text of a guardrail draft. It returns the list, or the
 * reason the text is not a JSON array of objects. The server checks each
 * detector; this check only keeps a typing error from reaching it.
 */
export function parseGuardrailDetectors(raw: string): { list: unknown[] } | { error: string } {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return {
      error: t('pages.admin.governance.field.detectorsJsonError', 'The detectors must be valid JSON.'),
    };
  }
  if (
    !Array.isArray(parsed) ||
    parsed.length === 0 ||
    parsed.some((entry) => typeof entry !== 'object' || entry === null || Array.isArray(entry))
  ) {
    return {
      error: t(
        'pages.admin.governance.field.detectorsShapeError',
        'The detectors must be a JSON array with one object or more.',
      ),
    };
  }
  return { list: parsed };
}

/** The `data.guardrail` document of a draft. A field left empty is omitted. */
export function guardrailDraftToData(draft: GuardrailDraft): Record<string, unknown> {
  const parsed = parseGuardrailDetectors(draft.guardrailDetectors);
  const guardrail: Record<string, unknown> = {
    stage: draft.guardrailStage,
    detectors: 'list' in parsed ? parsed.list : [],
  };
  const timeout = Number.parseInt(draft.guardrailTimeoutMs, 10);
  if (Number.isFinite(timeout)) guardrail.timeout_ms = timeout;
  if (draft.guardrailFailMode !== '') guardrail.fail_mode = draft.guardrailFailMode;
  // Only an input row can apply to embeddings; the server refuses the flag on an output row.
  if (draft.guardrailStage === 'input' && draft.guardrailApplyToEmbeddings) guardrail.apply_to_embeddings = true;
  // The look-behind applies to a streamed answer, so only an output row sends it.
  const window = Number.parseInt(draft.guardrailStreamWindow, 10);
  if (draft.guardrailStage === 'output' && Number.isFinite(window)) guardrail.stream_window_bytes = window;
  return guardrail;
}

function text(source: Record<string, unknown>, key: string, fallback = ''): string {
  const value = source[key];
  if (typeof value === 'string') return value;
  if (typeof value === 'number') return String(value);
  return fallback;
}

/** The guardrail fields of a draft, from a stored `data.guardrail` document. */
export function guardrailDraftFromData(guardrail: Record<string, unknown>): GuardrailDraft {
  return {
    guardrailStage: text(guardrail, 'stage', 'input'),
    guardrailDetectors: Array.isArray(guardrail.detectors)
      ? JSON.stringify(guardrail.detectors, null, 2)
      : DEFAULT_GUARDRAIL_DETECTORS,
    guardrailTimeoutMs: text(guardrail, 'timeout_ms'),
    guardrailFailMode: text(guardrail, 'fail_mode'),
    guardrailApplyToEmbeddings: guardrail.apply_to_embeddings === true,
    guardrailStreamWindow: text(guardrail, 'stream_window_bytes'),
  };
}
