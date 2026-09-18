/**
 * The form model shared by Settings › AI Personality and Settings › Memory.
 *
 * Both pages are one Formik form over the SAME record — the author profile —
 * and both save it with `PUT /social/author`. Baseline:
 * `EliteaUI/src/[fsd]/features/settings/lib/helpers/profile.helpers.js:17-95`.
 *
 * Two things about this backend shape the module:
 *
 *  1. `default_context_management` / `default_summarization` are TOP-LEVEL
 *     keys, their own two jsonb columns on centry.social_users
 *     (`MemoryContextManagement` / `MemorySummarization` in the generated
 *     model). They used to be nested inside `personalization` here, because
 *     `UpdateAuthor` read only `name`/`description`/`avatar`/
 *     `personalization` and silently dropped every other top-level key —
 *     nesting them was the only way anything survived the round trip. The
 *     handler now reads and writes the columns, so the nesting is gone; it is
 *     still ACCEPTED on the wire, so a client that has not shipped yet keeps
 *     working, and `serializeSettingsProfile` still reads it as a fallback for
 *     a profile last written that way.
 *  2. The PUT REPLACES the whole `personalization` blob (the upsert's
 *     `personalization = EXCLUDED.personalization`), and
 *     `name`/`description`/`avatar` are upserted from the body too, so
 *     omitting them blanks the stored avatar/description. `buildAuthorUpdate`
 *     below carries all of it forward. The two memory blocks are the
 *     exception: the handler KEEPS the stored value when the body does not
 *     mention them, which is what lets Settings › AI Personality save without
 *     carrying Settings › Memory's fields.
 */
import { DEFAULT_CONTEXT_STRATEGY } from '@/features/settings/lib/profile/context-budget/constants';

import { DEFAULT_PERSONA, emptyPersonalityInstructions } from './personaOptions';

/** `enable_context_editing`'s default — baseline `context-budget/lib/constants.js:22`. */
const DEFAULT_ENABLE_CONTEXT_EDITING = false;

/** `DEFAULT_MAX_TOKENS_CUSTOM` — baseline `shared/lib/constants/llmSettings.constants.js:8`. */
const DEFAULT_TARGET_SUMMARY_TOKENS = 4096;

/** A numeric field is briefly `''` while the user clears it (see `handleConvertToNumberChange`). */
type NumericFieldValue = number | '';

interface SummaryLlmSettings {
  instructions: string;
  model_name: string;
  model_project_id: string | null;
  max_tokens: NumericFieldValue;
}

export interface SettingsProfileFormValues {
  persona: string;
  /** Instructions are stored PER PERSONA, keyed by persona id (baseline #5392). */
  personality_instructions: Record<string, string>;
  context_enabled: boolean;
  max_context_tokens: NumericFieldValue;
  preserve_recent_messages: NumericFieldValue;
  enable_context_editing: boolean;
  enable_summarization: boolean;
  summary_llm_settings: SummaryLlmSettings;
}

/** The author record as this app reads it back from `GET /social/author`. */
export interface AuthorProfile {
  name?: string;
  description?: string;
  avatar?: string;
  personalization?: unknown;
  /** Absent for an account that has never saved Settings › Memory. */
  default_context_management?: Record<string, unknown>;
  /** Absent for an account that has never saved Settings › Memory. */
  default_summarization?: Record<string, unknown>;
}

interface Personalization {
  persona?: string;
  personality_instructions?: Record<string, string>;
  default_context_management?: Record<string, unknown>;
  default_summarization?: Record<string, unknown>;
}

function readPersonalization(author: AuthorProfile | undefined): Personalization {
  const raw = author?.personalization;
  if (raw === null || typeof raw !== 'object') return {};
  return raw;
}

/**
 * Reads one memory block, preferring the top-level column over the
 * personalization-nested copy an older client may have left behind.
 *
 * The fallback is not decoration: a profile last saved before this app stopped
 * nesting them carries the values ONLY in `personalization`, and dropping the
 * fallback would show that user the platform defaults as if they had never
 * configured anything. Their next save lifts the block into the column.
 */
function readMemoryBlock(
  author: AuthorProfile | undefined,
  personalization: Personalization,
  key: 'default_context_management' | 'default_summarization',
): Record<string, unknown> {
  const top = author?.[key];
  if (top !== null && typeof top === 'object') return top;
  const nested = personalization[key];
  if (nested !== null && typeof nested === 'object') return nested;
  return {};
}

function asNumber(value: unknown, fallback: number): number {
  return typeof value === 'number' ? value : fallback;
}

function asBoolean(value: unknown, fallback: boolean): boolean {
  return typeof value === 'boolean' ? value : fallback;
}

function asString(value: unknown, fallback: string): string {
  return typeof value === 'string' ? value : fallback;
}

function serializeSummarization(
  s: Record<string, unknown>,
  fallbackProjectId: string | null,
): { enable_summarization: boolean; summary_llm_settings: SummaryLlmSettings } {
  const projectId = s.summary_model_project_id;
  return {
    enable_summarization: asBoolean(s.enable_summarization, DEFAULT_CONTEXT_STRATEGY.ENABLE_SUMMARIZATION),
    summary_llm_settings: {
      instructions: asString(s.summary_instructions, ''),
      model_name: asString(s.summary_model_name, ''),
      model_project_id: typeof projectId === 'string' ? projectId : fallbackProjectId,
      max_tokens: asNumber(s.target_summary_tokens, DEFAULT_TARGET_SUMMARY_TOKENS),
    },
  };
}

/**
 * `GET /social/author` → Formik values.
 *
 * The per-persona map is merged OVER a full set of empty slots so every
 * persona key always exists, exactly as the baseline does — a persona whose
 * key is missing from the stored map must still show an empty editable field,
 * not `undefined`.
 */
export function serializeSettingsProfile(
  author: AuthorProfile | undefined,
  fallbackProjectId?: string,
): SettingsProfileFormValues {
  const p = readPersonalization(author);
  const cm = readMemoryBlock(author, p, 'default_context_management');
  const s = readMemoryBlock(author, p, 'default_summarization');

  return {
    persona: asString(p.persona, DEFAULT_PERSONA),
    personality_instructions: { ...emptyPersonalityInstructions(), ...p.personality_instructions },
    context_enabled: asBoolean(cm.enabled, DEFAULT_CONTEXT_STRATEGY.ENABLED),
    max_context_tokens: asNumber(cm.max_context_tokens, DEFAULT_CONTEXT_STRATEGY.MAX_CONTEXT_TOKENS),
    preserve_recent_messages: asNumber(
      cm.preserve_recent_messages,
      DEFAULT_CONTEXT_STRATEGY.PRESERVE_RECENT_MESSAGES,
    ),
    enable_context_editing: asBoolean(cm.enable_context_editing, DEFAULT_ENABLE_CONTEXT_EDITING),
    ...serializeSummarization(s, fallbackProjectId ?? null),
  };
}

/** Formik values → the `personalization` blob (the AI Personality page's fields). */
export function deserializeSettingsProfile(values: SettingsProfileFormValues): Record<string, unknown> {
  return {
    persona: values.persona,
    personality_instructions: values.personality_instructions,
  };
}

/**
 * Formik values → the two top-level memory blocks.
 *
 * A field the user has cleared is `''` (`handleConvertToNumberChange`), and an
 * empty string is not a number. It is OMITTED rather than sent: the blocks are
 * a defaults document, where "not set" is a real answer, and the server reads
 * an absent key as exactly that.
 */
export function deserializeMemoryBlocks(values: SettingsProfileFormValues): {
  default_context_management: Record<string, unknown>;
  default_summarization: Record<string, unknown>;
} {
  const s = values.summary_llm_settings;
  return {
    default_context_management: {
      enabled: values.context_enabled,
      ...numberField('max_context_tokens', values.max_context_tokens),
      ...numberField('preserve_recent_messages', values.preserve_recent_messages),
      enable_context_editing: values.enable_context_editing,
    },
    default_summarization: {
      enable_summarization: values.enable_summarization,
      summary_instructions: s.instructions,
      summary_model_name: s.model_name,
      ...projectIdField(s.model_project_id),
      ...numberField('target_summary_tokens', s.max_tokens),
    },
  };
}

function numberField(name: string, value: NumericFieldValue): Record<string, number> {
  return value === '' ? {} : { [name]: value };
}

/**
 * `summary_model_project_id` is the one field in this whole form (#929) that
 * the wire genuinely wants as an int (`MemorySummarization.summary_model_
 * project_id` — `zod.int()`, `internal/domain/contextsettings/userdefaults.go`'s
 * `*int`) even though project ids are STRINGS everywhere else in this app.
 * `model_project_id` in Formik state stays the string id (it is read back
 * and re-displayed as one, and `serializeSummarization`'s fallback is the
 * route's string `projectId` prop) — only the OUTGOING PUT needs the numeric
 * form, so the conversion happens here, at the wire boundary, not in the
 * form's own type. A value that is not a numeric string (null, or a
 * non-numeric project id this app never actually produces) is OMITTED
 * rather than sent malformed — the server keeps the stored value for an
 * absent key, which is the closest honest answer.
 */
function projectIdField(value: string | null): Record<string, number> {
  if (value === null || value === '') return {};
  const id = Number(value);
  return Number.isInteger(id) ? { summary_model_project_id: id } : {};
}

/**
 * The full `PUT /social/author` body: the edited fields laid over everything
 * the fetched profile already carried. See this module's header for why every
 * one of those carry-forwards is load-bearing against this backend.
 */
export function buildAuthorUpdate(
  author: AuthorProfile | undefined,
  values: SettingsProfileFormValues,
): {
  name?: string;
  description?: string;
  avatar?: string;
  personalization: Record<string, unknown>;
  default_context_management: Record<string, unknown>;
  default_summarization: Record<string, unknown>;
} {
  const { default_context_management: nestedContext, default_summarization: nestedSummary, ...personalization } =
    readPersonalization(author);
  // `nestedContext`/`nestedSummary` are destructured out, not spread back in:
  // the blocks now travel at the top level, and leaving a stale nested copy in
  // `personalization` would give the same setting two homes that drift apart.
  void nestedContext;
  void nestedSummary;

  return {
    ...(author?.name === undefined ? {} : { name: author.name }),
    ...(author?.description === undefined ? {} : { description: author.description }),
    ...(author?.avatar === undefined ? {} : { avatar: author.avatar }),
    personalization: {
      ...personalization,
      ...deserializeSettingsProfile(values),
    },
    ...deserializeMemoryBlocks(values),
  };
}
