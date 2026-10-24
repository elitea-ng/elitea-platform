import { describe, expect, it } from 'vitest';

import type { CustomCommand, StoredTurn } from '@/shared/desktop/workspaceIpc';

import { recordedPrompt } from './ThreadComposer';
import type { ComposerSkill } from './useAgentSkills';

const turn = (prompt: string): StoredTurn => ({ turn_id: 't', conversation_id: '1', conversation_uuid: null, prompt, mentions: ['a.txt'], started_at: 0, finished_at: 0, events: [], changes: null, events_truncated: false, state: 'done', live: true });
const local = (prompt: string) => ({ kind: 'local' as const, turns: [turn(prompt)] });
const COMMANDS: CustomCommand[] = [{ name: 'review', description: null, argument_hint: null, source: 'workspace', path: '.elitea/commands/review.md', ignored: [] }];
const SKILLS: ComposerSkill[] = [{ id: '7', name: 'style', description: 'The house style' }];

describe('recordedPrompt', () => {
  it('reads the skill or custom command of the recorded prompt again, as a send does', () => {
    expect(recordedPrompt(local('/style tidy it') as never, COMMANDS, SKILLS)).toEqual({ prompt: '/style tidy it', mentions: ['a.txt'], skills: ['style'], command: undefined });
    expect(recordedPrompt(local('/review the diff') as never, COMMANDS, SKILLS)).toEqual({ prompt: '/review the diff', mentions: ['a.txt'], skills: [], command: { name: 'review', arguments: 'the diff' } });
    expect(recordedPrompt(local('plain words') as never, COMMANDS, SKILLS)?.skills).toEqual([]);
    expect(recordedPrompt({ kind: 'none' } as never, COMMANDS, SKILLS)).toBeNull();
  });
});
