/**
 * The fields "Edit with AI" proposes and applies.
 *
 * The `edit_application_draft` Service Prompt asks the model for a COMPLETE
 * agent configuration as one JSON object (`name`, `description`,
 * `instructions`, `welcome_message`, `conversation_starters`, `suggested_*`).
 * Dropping that text into the Instructions field (the previous behaviour)
 * put raw JSON in the system prompt, so the response is parsed here and each
 * key is applied to its own field. `suggested_*` resources are never applied:
 * what is attached to the agent stays attached.
 */
export interface AiEditFields {
  readonly name: string;
  readonly description: string;
  readonly instructions: string;
  readonly welcome_message: string;
  readonly conversation_starters: readonly string[];
}

const FIELD_KEYS = ['name', 'description', 'instructions', 'welcome_message', 'conversation_starters'] as const;

/** The JSON object inside a completion, tolerating a ```json fence or prose around it. */
function jsonObjectText(content: string): string {
  const fenced = /```(?:json)?\s*([\s\S]*?)```/.exec(content);
  const body = (fenced?.[1] ?? content).trim();
  const start = body.indexOf('{');
  const end = body.lastIndexOf('}');
  return start >= 0 && end > start ? body.slice(start, end + 1) : body;
}

function textOr(value: unknown, fallback: string): string {
  return typeof value === 'string' ? value : value === null ? '' : fallback;
}

function startersOr(value: unknown, fallback: readonly string[]): readonly string[] {
  if (value === null) return [];
  if (!Array.isArray(value)) return fallback;
  return value.filter((item): item is string => typeof item === 'string');
}

/**
 * The proposed fields, or `undefined` when the completion is not a JSON
 * object carrying any of them (plain rewritten text then stands for the
 * instructions alone). Keys the model left out keep their current value.
 */
export function parseAiEditDraft(content: string, current: AiEditFields): AiEditFields | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(jsonObjectText(content));
  } catch {
    return undefined;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return undefined;
  const record = parsed as Record<string, unknown>;
  if (!FIELD_KEYS.some((key) => key in record)) return undefined;
  return {
    name: textOr(record['name'], current.name),
    description: textOr(record['description'], current.description),
    instructions: textOr(record['instructions'], current.instructions),
    welcome_message: textOr(record['welcome_message'], current.welcome_message),
    conversation_starters: startersOr(record['conversation_starters'], current.conversation_starters),
  };
}

/** The prompt sent to the model: the Service Prompt's placeholders filled, then the request. */
export function buildAiEditInput(basePrompt: string, current: AiEditFields, request: string): string {
  if (!basePrompt.includes('{current_config}')) {
    return `${basePrompt}\n\nCURRENT INSTRUCTIONS:\n${current.instructions}\n\nREQUESTED CHANGE:\n${request}`;
  }
  // The prompt is a Python format string: `{{`/`}}` are literal braces.
  // Unescape BEFORE substituting, and inject the agent's own text LAST, so
  // braces or placeholder-looking words inside it survive untouched.
  const filled = basePrompt
    .replaceAll('{{', '{')
    .replaceAll('}}', '}')
    .replaceAll('{toolkits}', '[]')
    .replaceAll('{agents}', '[]')
    .replaceAll('{skills}', '[]')
    .replaceAll('{current_config}', () => JSON.stringify(current, null, 2));
  return `${filled}\n\nREQUESTED CHANGE:\n${request}`;
}
