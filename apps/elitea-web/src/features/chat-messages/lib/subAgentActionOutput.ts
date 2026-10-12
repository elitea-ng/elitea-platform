import { convertJsonToString } from '@/shared/lib/json';

const JSON_FENCE = /^```json\n([\s\S]*)\n```$/;

/**
 * Preview text of one action in a sub-agent group.
 *
 * A saved agent or pipeline closes its card with `{"response": text}` (a
 * durable pipeline call adds the child's mapped `values`). The preview shows
 * that text, the child's own result, and a result the runtime fenced as JSON
 * shows as the JSON itself. Every other output keeps the baseline rendering.
 */
export function subAgentActionOutputText(output: unknown): string {
  let value = output;
  if (typeof output === 'string') {
    try {
      value = JSON.parse(output) as unknown;
    } catch {
      return output;
    }
  }
  if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
    const record = value as Record<string, unknown>;
    const keys = Object.keys(record);
    if (typeof record['response'] === 'string' && keys.every(key => key === 'response' || key === 'values')) {
      return JSON_FENCE.exec(record['response'])?.[1] ?? record['response'];
    }
  }
  return convertJsonToString(output ?? '');
}
