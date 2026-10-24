import { describe, expect, it } from 'vitest';

import { nameToReplace } from './useAutoConversationName';

const question = 'How do I rotate the TLS certificate of the LLM gateway without downtime?';
const exchange = [
  { role: 'user', content: question },
  { role: 'assistant', content: 'Run the rotation job, then restart the pods one at a time.' },
];

describe('nameToReplace', () => {
  it('names a chat whose first exchange is complete and whose name is still the automatic one', () => {
    expect(nameToReplace({ id: 1, name: question.slice(0, 50), chat_history: exchange })).toBe(question.slice(0, 50));
    expect(nameToReplace({ id: 1, name: question.slice(0, 80), chat_history: exchange })).toBe(question.slice(0, 80));
    expect(nameToReplace({ id: 1, name: 'New Chat', chat_history: exchange })).toBe('New Chat');
  });

  it('leaves a renamed chat, a chat still answering and a longer chat alone', () => {
    expect(nameToReplace({ id: 1, name: 'TLS notes', chat_history: exchange })).toBeUndefined();
    expect(nameToReplace({ id: 1, name: 'New Chat', chat_history: [exchange[0], { role: 'assistant', content: '...', isStreaming: true }] })).toBeUndefined();
    expect(nameToReplace({ id: 1, name: 'New Chat', chat_history: [exchange[0]] })).toBeUndefined();
    expect(nameToReplace({ id: 1, name: question.slice(0, 50), chat_history: [...exchange, { role: 'user', content: 'and then?' }, exchange[1]] })).toBeUndefined();
    expect(nameToReplace(undefined)).toBeUndefined();
  });
});
