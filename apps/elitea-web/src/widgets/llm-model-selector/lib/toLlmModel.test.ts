/**
 * Moved here with the adapter itself (it was three cases inside
 * `widgets/chat-box/ui/ChatBox.helpers.test.ts`), plus the two the move made
 * worth pinning: `LLMModelsMenu` keys its rows and its selected-check on
 * `id`, so an `id` that comes back `undefined` for a catalogue that omits the
 * field would collapse every row onto one React key.
 */
import { describe, expect, it } from 'vitest';

import { toLlmModel } from './toLlmModel';

describe('toLlmModel', () => {
  it('uses name as id when id is undefined', () => {
    expect(toLlmModel({ name: 'claude' } as never).id).toBe('claude');
  });

  it('stringifies numeric id', () => {
    expect(toLlmModel({ id: 7, name: 'gpt' } as never).id).toBe('7');
  });

  it('includes optional boolean/number fields when present', () => {
    const result = toLlmModel({ id: 1, name: 'x', shared: true, supports_vision: false, max_output_tokens: 4096 } as never);
    expect(result.shared).toBe(true);
    expect(result.supports_vision).toBe(false);
    expect(result.max_output_tokens).toBe(4096);
  });

  it('omits an absent optional field rather than carrying an undefined value', () => {
    const result = toLlmModel({ name: 'gpt' } as never);
    expect('display_name' in result).toBe(false);
    expect('supports_reasoning' in result).toBe(false);
  });

  it('carries a trimmed description and drops a blank or non-text one', () => {
    expect(toLlmModel({ name: 'gpt', description: '  Fast for everyday tasks ' } as never).description).toBe('Fast for everyday tasks');
    expect('description' in toLlmModel({ name: 'gpt', description: '   ' } as never)).toBe(false);
    expect('description' in toLlmModel({ name: 'gpt', description: null } as never)).toBe(false);
    expect('description' in toLlmModel({ name: 'gpt', description: 7 } as never)).toBe(false);
  });

  it('ignores a catalogue field of the wrong type', () => {
    const result = toLlmModel({ name: 'gpt', shared: 'yes', max_output_tokens: '4096' } as never);
    expect('shared' in result).toBe(false);
    expect('max_output_tokens' in result).toBe(false);
  });
});
