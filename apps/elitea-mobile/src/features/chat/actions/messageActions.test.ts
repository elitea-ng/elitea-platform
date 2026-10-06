import { answerGroupBefore } from '@/api/__fixtures__/chat13';
import { SHARE_OUT_OFF, codeBlockLabel, codeBlocks, planActions } from '@/features/chat/actions/messageActions';

const answer = { role: 'assistant' as const, text: answerGroupBefore.content, uuid: answerGroupBefore.uuid, isStreaming: false };
const all = { shareOut: true, contract13: true, online: true, canSpeak: true, canRegenerate: true };

describe('codeBlocks', () => {
  it('finds the fenced block of a captured answer, with its language', () => {
    expect(codeBlocks(answer.text)).toEqual([{ language: 'python', code: "print('hi')" }]);
  });

  it('handles tildes, longer fences, indentation, an unclosed block and empty blocks', () => {
    const text = [
      'intro',
      '~~~',
      'a',
      '~~~',
      '  ````ts title="x"',
      '  const b = 1;',
      '   ```',
      '  inner',
      '  ````',
      '```',
      '```',
      '```sh',
      'tail',
    ].join('\n');
    expect(codeBlocks(text)).toEqual([
      { language: null, code: 'a' },
      { language: 'ts', code: 'const b = 1;\n ```\ninner' },
      { language: 'sh', code: 'tail' },
    ]);
  });

  it('labels blocks by language and number', () => {
    expect(codeBlockLabel({ language: 'python', code: '' }, 0, 1)).toBe('Copy python code');
    expect(codeBlockLabel({ language: null, code: '' }, 1, 2)).toBe('Copy code (2)');
  });
});

describe('planActions', () => {
  it('offers everything on an answer when the workspace allows it', () => {
    const plan = planActions(answer, all);
    expect(plan.actions).toEqual(['copy', 'copy_code', 'share', 'read_aloud', 'like', 'dislike', 'regenerate']);
    expect(plan.codeBlocks).toHaveLength(1);
    expect(plan.shareOutNote).toBeNull();
  });

  it('allow_share_out off: no copy, no code copy, no share, and the sheet says why', () => {
    const plan = planActions(answer, { ...all, shareOut: false });
    expect(plan.actions).toEqual(['read_aloud', 'like', 'dislike', 'regenerate']);
    expect(plan.codeBlocks).toEqual([]);
    expect(plan.shareOutNote).toBe(SHARE_OUT_OFF);
  });

  it('no feedback before client contract 1.3, offline, while streaming, or on a user message', () => {
    for (const context of [{ ...all, contract13: false }, { ...all, online: false }]) {
      expect(planActions(answer, context).actions).not.toContain('like');
    }
    expect(planActions({ ...answer, isStreaming: true }, all).actions).not.toContain('like');
    expect(planActions({ ...answer, uuid: null }, all).actions).not.toContain('dislike');
    expect(planActions({ ...answer, role: 'user' }, all).actions).toEqual(['copy', 'share', 'read_aloud']);
  });
});
