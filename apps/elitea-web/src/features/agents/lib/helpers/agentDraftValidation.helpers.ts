/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/agent/lib/helpers/agentDraftValidation.helpers.js`
 * (byte-for-byte validation logic). Limits now come from this app's
 * `shared/lib/limits.ts` (unit S3) instead of `common/constants.js` — same
 * values (verified: `shared/lib/limits.test.ts` asserts
 * `MAX_NAME_LENGTH === 32`, `MAX_DESCRIPTION_LENGTH === 2000`,
 * `MAX_CONVERSATION_STARTERS === 4`, `MAX_CONVERSATION_STARTER_LENGTH ===
 * 768`, `MAX_WELCOME_MESSAGE_LENGTH === 768`, matching
 * `apps/elitea-ui/src/common/constants.js:66-76`).
 */
import {
  MAX_CONVERSATION_STARTERS,
  MAX_CONVERSATION_STARTER_LENGTH,
  MAX_DESCRIPTION_LENGTH,
  MAX_NAME_LENGTH,
  MAX_WELCOME_MESSAGE_LENGTH,
} from '@/shared/lib/limits';

/** The shape of an AI-generated agent draft this validator checks (a subset of the real draft fields — only what is validated). */
export interface AgentDraft {
  readonly name?: string;
  readonly description?: string;
  readonly welcome_message?: string;
  readonly conversation_starters?: readonly (string | undefined)[];
}

/** Field-keyed error messages, one entry per failed rule — `{}` means the draft is valid. */
export interface AgentDraftErrors {
  readonly name?: string;
  readonly description?: string;
  readonly welcome_message?: string;
  readonly conversation_starters?: string;
  readonly conversation_starters_length?: string;
}

/**
 * Validates an AI-generated agent draft before it is applied to the create-
 * agent form. `agentDraftValidation.helpers.js:9-28`, ported line-for-line.
 */
export function validateAgentDraft(draft: AgentDraft): AgentDraftErrors {
  const errors: {
    name?: string;
    description?: string;
    welcome_message?: string;
    conversation_starters?: string;
    conversation_starters_length?: string;
  } = {};

  const name = (draft.name ?? '').trim();
  if (!name) errors.name = 'Name is required';
  else if (name.length > MAX_NAME_LENGTH) errors.name = `Name must be ${MAX_NAME_LENGTH} characters or less`;

  const description = (draft.description ?? '').trim();
  if (!description) errors.description = 'Description is required';
  else if (description.length > MAX_DESCRIPTION_LENGTH)
    errors.description = `Description must be ${MAX_DESCRIPTION_LENGTH} characters or less`;

  const welcomeMessage = draft.welcome_message ?? '';
  if (welcomeMessage.length > MAX_WELCOME_MESSAGE_LENGTH)
    errors.welcome_message = `Welcome message must be ${MAX_WELCOME_MESSAGE_LENGTH} characters or less`;

  const starters = draft.conversation_starters ?? [];
  if (starters.length > MAX_CONVERSATION_STARTERS)
    errors.conversation_starters = `Maximum ${MAX_CONVERSATION_STARTERS} conversation starters allowed`;
  if (starters.some((s) => s !== undefined && s.length > MAX_CONVERSATION_STARTER_LENGTH))
    errors.conversation_starters_length = `Each starter must be ${MAX_CONVERSATION_STARTER_LENGTH} characters or less`;

  return errors;
}
