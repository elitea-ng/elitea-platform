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

/**
 * The JSON object inside a completion. In order: the whole text; one OUTER
 * ```json fence anchored at both ends (an `instructions` value may itself hold
 * code fences, so the first fence is not the outer one); the span from the first
 * `{` to the last `}`.
 */
function parseJsonObject(content: string): unknown {
  const body = content.trim();
  const attempts: string[] = [body];
  const outer = /^```(?:json)?\s*([\s\S]*?)\s*```$/.exec(body);
  if (outer?.[1] !== undefined) attempts.push(outer[1]);
  const start = body.indexOf('{');
  const end = body.lastIndexOf('}');
  if (start >= 0 && end > start) attempts.push(body.slice(start, end + 1));
  for (const attempt of attempts) {
    try {
      return JSON.parse(attempt);
    } catch {
      // try the next shape
    }
  }
  return undefined;
}

/** A string, `null` (meaning "none") or absent; anything else is malformed. */
function optionalText(record: Record<string, unknown>, key: string): string | null | undefined {
  const value = record[key];
  if (value === undefined || value === null) return value;
  return typeof value === 'string' ? value : undefined;
}

function isWellFormed(record: Record<string, unknown>): boolean {
  const textKeys = ['name', 'description', 'instructions', 'welcome_message'];
  const textsOk = textKeys.every((key) => !(key in record) || record[key] === null || typeof record[key] === 'string');
  const starters = record['conversation_starters'];
  const startersOk = starters === undefined || starters === null || (Array.isArray(starters) && starters.every((item) => typeof item === 'string'));
  return textsOk && startersOk;
}

/**
 * The proposed fields, or `undefined` when the completion is not a well-formed
 * JSON object carrying any of them (plain text then stands for the instructions
 * alone). Keys the model left out, or set to `null`, keep their current value;
 * a blank `name` is never proposed.
 */
export function parseAiEditDraft(content: string, current: AiEditFields): AiEditFields | undefined {
  const parsed = parseJsonObject(content);
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return undefined;
  const record = parsed as Record<string, unknown>;
  if (!FIELD_KEYS.some((key) => key in record) || !isWellFormed(record)) return undefined;
  const name = optionalText(record, 'name');
  const starters = record['conversation_starters'];
  return {
    name: typeof name === 'string' && name.trim() !== '' ? name : current.name,
    description: optionalText(record, 'description') ?? current.description,
    instructions: optionalText(record, 'instructions') ?? current.instructions,
    welcome_message: optionalText(record, 'welcome_message') ?? current.welcome_message,
    conversation_starters: Array.isArray(starters) ? (starters as string[]) : current.conversation_starters,
  };
}

/**
 * What the user's review changed relative to the SNAPSHOT the prompt was built
 * from. Only these keys are written back, so a field edited in the page while
 * the model worked is not overwritten with a stale copy. Blank starters drop out.
 */
export function aiEditPatch(snapshot: AiEditFields, proposed: AiEditFields): Partial<AiEditFields> {
  const starters = proposed.conversation_starters.filter((starter) => starter.trim() !== '');
  const patch: { -readonly [K in keyof AiEditFields]?: AiEditFields[K] } = {};
  if (proposed.name.trim() !== '' && proposed.name !== snapshot.name) patch.name = proposed.name;
  if (proposed.description !== snapshot.description) patch.description = proposed.description;
  if (proposed.instructions !== snapshot.instructions) patch.instructions = proposed.instructions;
  if (proposed.welcome_message !== snapshot.welcome_message) patch.welcome_message = proposed.welcome_message;
  if (starters.join('\n') !== snapshot.conversation_starters.join('\n')) patch.conversation_starters = starters;
  return patch;
}

/** The prompt sent to the model: placeholders filled and escapes resolved in ONE pass, then the request. */
export function buildAiEditInput(basePrompt: string, current: AiEditFields, request: string): string {
  if (!basePrompt.includes('{current_config}')) {
    return `${basePrompt}\n\nCURRENT INSTRUCTIONS:\n${current.instructions}\n\nREQUESTED CHANGE:\n${request}`;
  }
  // The prompt is a Python format string: `{{`/`}}` are literal braces. One regex
  // pass, so an escaped `{{skills}}` stays literal and the agent's own text is
  // never rescanned. The attached resources are not known here, so the lists the
  // prompt offers are said to be unavailable rather than passed off as empty.
  const filled = basePrompt.replace(/\{\{|\}\}|\{(current_config|toolkits|agents|skills)\}/g, (match, name: string | undefined) => {
    if (match === '{{') return '{';
    if (match === '}}') return '}';
    return name === 'current_config' ? JSON.stringify(current, null, 2) : '(not provided)';
  });
  return `${filled}\n\nREQUESTED CHANGE:\n${request}`;
}
