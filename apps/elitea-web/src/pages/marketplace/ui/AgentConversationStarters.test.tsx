import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { AgentConversationStarters } from './AgentConversationStarters';

/**
 * elitea_issues package C-agents — #4932/#4933.
 *
 * The API's `conversation_starters` array is a loosely-typed jsonb
 * passthrough (see `ConversationStarters` in api/openapi/v2.yaml) — an agent
 * created/edited through the raw API (or a pre-existing row saved before the
 * Go handler filtered non-string entries, #4933) can carry `null`, a number,
 * or an object alongside real strings. The old `s?.trim()` filter only
 * guarded `null`/`undefined`; a number or object still threw
 * `TypeError: s.trim is not a function`, crashing this whole panel on the
 * Agent Hub page. Fixed with a `typeof s === 'string'` type guard first.
 */
describe('AgentConversationStarters', () => {
  it('does not throw and renders only the string entries when the array carries non-string values', () => {
    // Deliberately malformed, matching what the loosely-typed API can actually
    // return — cast through `unknown` rather than `any` for the lint rule.
    const starters = ['Real starter', null, 42, { bad: true }, 'Another one', '   '] as unknown as string[];

    expect(() => renderWithTheme(<AgentConversationStarters conversation_starters={starters} />)).not.toThrow();

    expect(screen.getByText('Real starter')).toBeInTheDocument();
    expect(screen.getByText('Another one')).toBeInTheDocument();
    expect(screen.queryByText('42')).not.toBeInTheDocument();
  });

  it('shows the empty-state message when every entry is filtered out', () => {
    const starters = [null, 42, '   '] as unknown as string[];

    renderWithTheme(<AgentConversationStarters conversation_starters={starters} />);

    expect(screen.getByText('No predefined conversation starters – just type your request to begin.')).toBeInTheDocument();
  });
});
