import { fireEvent, render, screen } from '@testing-library/react-native';
import { useState } from 'react';

import { fetchAgentSkills, fetchToolkitTools } from '@/api/queries/skillsTools';
import { Composer, type PickResult } from '@/features/chat/Composer';
import type { ActiveTrigger } from '@/features/chat/mentions/mentions';
import { skillInsert, slashPhase, toolInsert, toolkitInsert, type ToolkitOption } from '@/features/chat/slashPicker';
import type { Suggestion, SuggestionState } from '@/features/chat/SuggestionStrip';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const toolkits: ToolkitOption[] = [
  { toolkitId: 90, name: 'github' },
  { toolkitId: 91, name: 'Jira Cloud' },
];

describe('slashPhase (the two-phase state machine)', () => {
  it('lists toolkits until the second slash, then that toolkit tools', () => {
    expect(slashPhase('', toolkits)).toEqual({ phase: 'toolkit', filter: '' });
    expect(slashPhase('gi', toolkits)).toEqual({ phase: 'toolkit', filter: 'gi' });
    expect(slashPhase('GitHub/', toolkits)).toEqual({ phase: 'tool', toolkit: toolkits[0], filter: '' });
    expect(slashPhase('github/list', toolkits)).toEqual({ phase: 'tool', toolkit: toolkits[0], filter: 'list' });
    expect(slashPhase('gitlab/x', toolkits)).toEqual({ phase: 'unknown_toolkit', name: 'gitlab' });
  });

  it('inserts token-safe text', () => {
    expect(toolkitInsert(toolkits[1])).toBe('/Jira_Cloud/');
    expect(toolInsert(toolkits[0], 'list issues')).toBe('/github/list_issues ');
    expect(skillInsert('summarize')).toBe('~summarize ');
  });
});

describe('the composer with "/" (phase 1 → phase 2)', () => {
  function Harness({ onSend }: { onSend: jest.Mock }) {
    const [trigger, setTrigger] = useState<ActiveTrigger | null>(null);
    const phase = trigger ? slashPhase(trigger.query, toolkits) : null;
    const suggestions: SuggestionState | null =
      phase?.phase === 'toolkit'
        ? { items: toolkits.map((t) => ({ key: `toolkit-${t.toolkitId}`, title: t.name })) }
        : phase?.phase === 'tool'
          ? { items: [{ key: 'tool-list_issues', title: 'list_issues' }, { key: 'tool-create_issue', title: 'create_issue' }] }
          : null;
    const pick = (picked: ActiveTrigger, item: Suggestion): PickResult => {
      const current = slashPhase(picked.query, toolkits);
      if (current.phase === 'toolkit') return { insert: toolkitInsert(toolkits.find((t) => `toolkit-${t.toolkitId}` === item.key)!) };
      if (current.phase === 'tool') return { insert: toolInsert(current.toolkit, item.title) };
      return { error: 'no' };
    };
    return (
      <Composer
        busy={false}
        onSend={onSend}
        externalTriggers={['/']}
        onTriggerChange={setTrigger}
        externalSuggestions={suggestions}
        onPickExternal={pick}
      />
    );
  }

  it('picks a toolkit, then a tool, and sends the words', async () => {
    const onSend = jest.fn(() => null);
    await render(<Harness onSend={onSend} />);
    await fireEvent.changeText(screen.getByLabelText('Message'), 'run /gi');
    await fireEvent.press(await screen.findByLabelText('github'));
    expect(screen.getByLabelText('Message').props.value).toBe('run /github/');
    await fireEvent.press(await screen.findByLabelText('list_issues'));
    expect(screen.getByLabelText('Message').props.value).toBe('run /github/list_issues ');
    await fireEvent.press(screen.getByLabelText('Send'));
    expect(onSend).toHaveBeenCalledWith({ text: 'run /github/list_issues ', mentions: null });
  });
});

describe('skills and toolkit tools (1.1 reads)', () => {
  beforeEach(() => {
    __resetSessionForTests();
    setSession({ serverOrigin: FAKE_ORIGIN, projectId: 7 });
    setCredential({ kind: 'pat', token: 't' });
  });

  it('reads the union of `skills` and `items`, once per name', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/application_skills/prompt_lib/7/310', {
      status: 200,
      body: {
        items: [{ id: '4', name: 'summarize', description: 'Short summaries', project_id: '7', is_default: false, type: 'skill', created_at: '2026-10-05T09:00:00Z', updated_at: '0001-01-01T00:00:00Z' }],
        total: 1,
        page: 1,
        page_size: 50,
        total_pages: 1,
        max_skills: 5,
        skills: [{ skill_id: 4, name: 'summarize', description: 'Short summaries', version_name: 'base', version_missing: false }],
      },
    });
    await expect(fetchAgentSkills(7, 310, server.client)).resolves.toEqual([{ name: 'summarize', description: 'Short summaries' }]);
  });

  it("lists a toolkit's tools, and reads discovery switched off as not available", async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/toolkit_available_tools/prompt_lib/7/90', {
      status: 200,
      body: { tools: [{ name: 'list_issues', description: 'List issues' }, { name: '', description: 'x' }], args_schemas: {} },
    });
    await expect(fetchToolkitTools(7, 90, server.client)).resolves.toEqual([{ name: 'list_issues', description: 'List issues' }]);
    server.on('GET', '/elitea_core/toolkit_available_tools/prompt_lib/7/90', { status: 503, body: { error: 'discovery is not enabled' } });
    await expect(fetchToolkitTools(7, 90, server.client)).resolves.toBeNull();
  });
});
