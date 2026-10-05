/**
 * Prompt/application payload-key mapping ported from
 * apps/elitea-ui/src/common/constants.js:1030-1056 (unit S3, spec §9.3).
 *
 * The old app carries a `// todo: delete this` comment on this export, but
 * it has 10 live consumers (grep-verified) — NOT dead code; the TODO is
 * preserved below as-authored rather than silently dropped or acted on.
 */

// todo: delete this
export const PROMPT_PAYLOAD_KEY = {
  name: 'name',
  type: 'type',
  description: 'description',
  tags: 'tags',
  context: 'prompt',
  messages: 'messages',
  variables: 'variables',
  modelName: 'model_name',
  temperature: 'temperature',
  maxTokens: 'max_tokens',
  reasoningEffort: 'reasoning_effort',
  stepsLimit: 'steps_limit',
  integrationUid: 'integration_uid',
  integrationName: 'integration_name',
  ownerId: 'owner_id',
  is_liked: 'is_liked',
  likes: 'likes',
  welcomeMessage: 'welcome_message',
  conversationStarters: 'conversation_starters',
  rejectDetails: 'reject_details',
  allowAttachment: 'allow_attachment',
  meta: 'meta',
  isForked: 'is_forked',
  icon: 'icon',
} as const;

/** `constants.js:126-132`. */
export const APPLICATION_PAYLOAD_KEY = {
  file: 'file',
  name: 'name',
  description: 'description',
  tags: 'tags',
  type: 'type',
} as const;
