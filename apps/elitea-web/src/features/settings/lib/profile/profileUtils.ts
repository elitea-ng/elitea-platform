import * as yup from 'yup';
import { resolveContextBudgetMode, type ContextBudgetMode } from '@/shared/lib/contextBudget';

import { DEFAULT_CONTEXT_STRATEGY, SEPARATOR, VALIDATION_LIMITS } from './context-budget/constants';

export const PROFILE_INITIAL_VALUES = {
  persona: 'generic',
  default_instructions: '',
  context_enabled: DEFAULT_CONTEXT_STRATEGY.ENABLED,
  budget_mode: 'balanced' as ContextBudgetMode,
  preserve_recent_messages: DEFAULT_CONTEXT_STRATEGY.PRESERVE_RECENT_MESSAGES,
  enable_summarization: DEFAULT_CONTEXT_STRATEGY.ENABLE_SUMMARIZATION,
  summary_llm_settings: {
    instructions: '',
    model_name: '',
    model_project_id: null as string | null,
    max_tokens: 4096,
  },
};

/**
 * Shape of `GET /social/author`'s `personalization` field.
 *
 * `services/elitea-main/internal/api/v2/social/handler.go`'s `AuthorResponse`
 * (`GetAuthor`) has exactly one settings-carrying column: `personalization`
 * (an arbitrary jsonb blob, `centry.social_users.personalization`). There is
 * no `default_context_management`/`default_summarization` column or response
 * field at all — unlike the old pylon-backed app, which had those as
 * separate top-level keys on both the author response and the
 * `AuthorUpdateRequest` PUT body. `UpdateAuthor` (same file) decodes the PUT
 * body into `map[string]any` and reads only `name`/`description`/`avatar`/
 * `personalization` (`strVal`/`jsonVal` helpers) — any other top-level key is
 * silently dropped, never persisted. So context-management and
 * summarization settings have to live NESTED inside `personalization` here
 * to survive a save at all.
 */
interface SocialAuthorPersonalization {
  persona?: string;
  default_instructions?: string;
  default_context_management?: Record<string, unknown>;
  default_summarization?: Record<string, unknown>;
}

interface SocialAuthorData {
  name?: string;
  email?: string;
  avatar?: string;
  description?: string;
  personalization?: SocialAuthorPersonalization;
}

// ---------------------------------------------------------------------------
// Helper: serialize personalization (complexity ≤ 3)
// ---------------------------------------------------------------------------

function serializePersonalization(p: SocialAuthorPersonalization): Record<string, string> {
  return {
    persona: p.persona || 'generic',
    default_instructions: p.default_instructions || '',
  };
}

// ---------------------------------------------------------------------------
// Helper: serialize context management (complexity ≤ 5)
// ---------------------------------------------------------------------------

function serializeContextManagement(cm: Record<string, unknown>): Record<string, unknown> {
  return {
    context_enabled: cm.enabled ?? DEFAULT_CONTEXT_STRATEGY.ENABLED,
    budget_mode: resolveContextBudgetMode(cm.budget_mode),
    preserve_recent_messages: cm.preserve_recent_messages ?? DEFAULT_CONTEXT_STRATEGY.PRESERVE_RECENT_MESSAGES,
  };
}

// ---------------------------------------------------------------------------
// Helper: serialize summarization (complexity ≤ 6)
// ---------------------------------------------------------------------------

function serializeSummarization(
  s: Record<string, unknown>,
  defaultModel: { name: string; project_id: string; default?: boolean; display_name?: string } | null,
): Record<string, unknown> {
  return {
    enable_summarization: s.enable_summarization ?? DEFAULT_CONTEXT_STRATEGY.ENABLE_SUMMARIZATION,
    summary_llm_settings: {
      instructions: s.summary_instructions || '',
      model_name: s.summary_model_name || defaultModel?.name || '',
      model_project_id: s.summary_model_project_id ?? defaultModel?.project_id ?? null,
      max_tokens: ((s.target_summary_tokens ?? 4096) as number),
    },
  };
}

// ---------------------------------------------------------------------------
// Helper: serialize no-author-data case (complexity ≤ 3)
// ---------------------------------------------------------------------------

function serializeEmptyProfile(
  defaultModel: { name: string; project_id: string; default?: boolean; display_name?: string } | null,
): typeof PROFILE_INITIAL_VALUES {
  return {
    ...PROFILE_INITIAL_VALUES,
    persona: '',
    summary_llm_settings: {
      ...PROFILE_INITIAL_VALUES.summary_llm_settings,
      model_name: defaultModel?.name || '',
      model_project_id: defaultModel?.project_id ?? null,
    },
  };
}

export function serializeProfileFormData(
  authorData: SocialAuthorData | undefined,
  defaultModel: { name: string; project_id: string; default?: boolean; display_name?: string } | null,
) {
  if (!authorData) {
    return serializeEmptyProfile(defaultModel);
  }

  const p = authorData.personalization || {};
  const cm = p.default_context_management || {};
  const s = p.default_summarization || {};

  return {
    ...serializePersonalization(p),
    ...serializeContextManagement(cm),
    ...serializeSummarization(s, defaultModel),
  };
}

/**
 * Builds the `PUT /social/author` body. Everything the form edits
 * (persona/instructions/context-management/summarization) is nested inside
 * `personalization` — see `SocialAuthorPersonalization`'s doc comment above
 * for why: the Go handler only reads `name`/`description`/`avatar`/
 * `personalization` from the body and silently drops any other top-level
 * key, so a flat `default_context_management`/`default_summarization`
 * payload (the old pylon-app's wire shape) would save nothing for those
 * fields against this backend.
 */
export function deserializeProfileFormData(formValues: Record<string, unknown>): Record<string, unknown> {
  const s = formValues.summary_llm_settings as Record<string, unknown> | undefined;
  return {
    personalization: {
      persona: formValues.persona,
      default_instructions: formValues.default_instructions,
      default_context_management: {
        enabled: formValues.context_enabled,
        budget_mode: formValues.budget_mode,
        preserve_recent_messages: formValues.preserve_recent_messages,
      },
      default_summarization: {
        enable_summarization: formValues.enable_summarization,
        summary_instructions: s?.instructions,
        summary_model_name: s?.model_name,
        summary_model_project_id: s?.model_project_id,
        target_summary_tokens: s?.max_tokens,
      },
    },
  };
}

/**
 * Reshape the flat Formik profile values into the nested `formData` prop
 * `ContextStrategySummarization` takes. Baseline:
 * `pages/UserSettings/profileUtils.js:97`.
 *
 * [#71] Signature tightened from `(Record<string, unknown>) =>
 * Record<string, unknown>` to the real `ProfileFormValues` shapes when its
 * first caller was wired up: the old signature erased every field type, so the
 * consumer could not accept the result without a cast.
 */
export function createContextStrategyFormData(formikValues: ProfileFormValues): {
  enabled: boolean;
  budget_mode: ContextBudgetMode;
  preserve_recent_messages: number;
  enable_summarization: boolean;
  summary_llm_settings: ProfileFormValues['summary_llm_settings'];
} {
  return {
    enabled: formikValues.context_enabled,
    budget_mode: formikValues.budget_mode,
    preserve_recent_messages: formikValues.preserve_recent_messages,
    enable_summarization: formikValues.enable_summarization,
    summary_llm_settings: formikValues.summary_llm_settings,
  };
}

export function parseModelValue(value: string): { modelName: string; modelProjectId: number | null } {
  const parts = value.split(SEPARATOR);
  return {
    modelName: parts[0] ?? '',
    modelProjectId: parts[1] ? Number(parts[1]) : null,
  };
}

/** Initial form values for the profile settings form. */
export type ProfileFormValues = typeof PROFILE_INITIAL_VALUES;

// eslint-disable-next-line @typescript-eslint/no-explicit-any, @typescript-eslint/no-unsafe-assignment
export const ProfileValidationSchema: yup.ObjectSchema<ProfileFormValues> =
  yup.object({
    persona: yup.string().required('Please select a personality'),
    default_instructions: yup.string().notRequired(),
    context_enabled: yup.boolean().notRequired(),
    budget_mode: yup.string().oneOf(['balanced', 'full']).required(),
    preserve_recent_messages: yup
      .number()
      .typeError('Please enter a valid number')
      .integer('Must be a whole number')
      .when('context_enabled', {
        is: true,
        // oxlint-disable-next-line unicorn/no-thenable -- yup's ConditionOptions `then` key, not a Promise thenable.
        then: (schema) =>
          schema
            .required('This field is required')
            .min(
              VALIDATION_LIMITS.PRESERVE_RECENT_MESSAGES.MIN,
              `Preserve messages must be at least ${VALIDATION_LIMITS.PRESERVE_RECENT_MESSAGES.MIN}`,
            )
            .max(
              VALIDATION_LIMITS.PRESERVE_RECENT_MESSAGES.MAX,
              `Preserve messages cannot exceed ${VALIDATION_LIMITS.PRESERVE_RECENT_MESSAGES.MAX}`,
            ),
        otherwise: (schema) => schema.nullable(),
      }),
    enable_summarization: yup.boolean().notRequired(),
    summary_llm_settings: yup.object({
      instructions: yup.string(),
      model_name: yup.string(),
      model_project_id: yup.number().nullable(),
      max_tokens: yup
        .number()
        .typeError('Please enter a valid number')
        .integer('Must be a whole number')
        .required('This field is required')
        .min(
          VALIDATION_LIMITS.MAX_TOKENS.MIN,
          `Target tokens must be at least ${VALIDATION_LIMITS.MAX_TOKENS.MIN}`,
        )
        .max(
          VALIDATION_LIMITS.MAX_TOKENS.MAX,
          `Target tokens cannot exceed ${VALIDATION_LIMITS.MAX_TOKENS.MAX.toLocaleString()}`,
        ),
    }),
  }) as any;
