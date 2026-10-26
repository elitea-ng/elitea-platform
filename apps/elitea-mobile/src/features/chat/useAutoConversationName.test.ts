import { nameToReplace } from '@/features/chat/useAutoConversationName';

const question = 'How do I rotate the TLS certificate of the LLM gateway without downtime?';
const exchange = [
  { role: 'user' as const, text: question, isStreaming: false },
  { role: 'assistant' as const, text: 'Run the rotation job, then restart the pods.', isStreaming: false },
];
const agent = { id: 3, entity_name: 'application', meta: { name: 'Ops helper' } };

describe('nameToReplace', () => {
  it('replaces the agent’s name, a cut of the first message or a default once the first answer is in', () => {
    expect(nameToReplace({ name: 'Ops helper', participants: [agent] } as never, exchange)).toBe('Ops helper');
    expect(nameToReplace({ name: question.slice(0, 50), participants: [] } as never, exchange)).toBe(question.slice(0, 50));
    expect(nameToReplace({ name: 'New Chat', participants: [] } as never, exchange)).toBe('New Chat');
  });

  it('keeps a renamed chat, a chat still answering and a longer chat', () => {
    expect(nameToReplace({ name: 'TLS notes', participants: [agent] } as never, exchange)).toBeNull();
    expect(nameToReplace({ name: 'Ops helper', participants: [agent] } as never, [exchange[0]!, { role: 'assistant', text: '', isStreaming: true }])).toBeNull();
    expect(nameToReplace({ name: 'Ops helper', participants: [agent] } as never, [...exchange, { role: 'user', text: 'and then?', isStreaming: false }, exchange[1]!])).toBeNull();
    expect(nameToReplace(undefined, exchange)).toBeNull();
  });
});
