import { describe, expect, it, vi } from 'vitest';

import { threadSkillCommands } from './threadSkillsIpc';

describe('threadSkillCommands', () => {
  it('lists the pins as camelCase and sends the host its snake_case arguments', async () => {
    const invoke = vi.fn((command: string): Promise<unknown> => (command === 'thread_skills_list' ? Promise.resolve([{ project_id: 7, skill_id: 12 }]) : command === 'thread_skill_unpin' ? Promise.resolve(true) : Promise.resolve(null)));
    const commands = threadSkillCommands(invoke as never);
    expect(await commands.threadSkills('w1', '42')).toEqual([{ projectId: 7, skillId: 12 }]);
    await commands.pinThreadSkill('w1', '42', { projectId: 7, skillId: 12 });
    expect(await commands.unpinThreadSkill('w1', '42', 12)).toBe(true);
    expect(invoke.mock.calls).toEqual([
      ['thread_skills_list', { workspace_id: 'w1', conversation_id: '42' }],
      ['thread_skill_pin', { workspace_id: 'w1', conversation_id: '42', project_id: 7, skill_id: 12 }],
      ['thread_skill_unpin', { workspace_id: 'w1', conversation_id: '42', skill_id: 12 }],
    ]);
  });
});
