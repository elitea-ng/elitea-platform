import { describe, expect, it } from 'vitest';

import { subAgentActionOutputText } from './subAgentActionOutput';

describe('subAgentActionOutputText', () => {
  it("shows a saved child's response text, not its JSON envelope", () => {
    expect(subAgentActionOutputText('{"response":"LEFT via ONCE"}')).toBe('LEFT via ONCE');
    expect(subAgentActionOutputText({ response: 'done', values: { reply: 'done' } })).toBe('done');
  });

  it('shows a fenced JSON result as the JSON itself', () => {
    const fenced = '```json\n[\n  {\n    "id": "A"\n  }\n]\n```';
    expect(subAgentActionOutputText(JSON.stringify({ response: fenced }))).toBe('[\n  {\n    "id": "A"\n  }\n]');
  });

  it('keeps every other output as before', () => {
    expect(subAgentActionOutputText('plain text')).toBe('plain text');
    expect(subAgentActionOutputText('{"response":"x","extra":1}')).toBe('{"response":"x","extra":1}');
    expect(subAgentActionOutputText({ items: [1] })).toBe('{\n  "items": [\n    1\n  ]\n}');
    expect(subAgentActionOutputText(undefined)).toBe('');
    expect(subAgentActionOutputText('{"response":["a"]}')).toBe('{"response":["a"]}');
  });
});
