import { describe, expect, it } from 'vitest';

import type { CustomCommand } from '@/shared/desktop/workspaceIpc';

import { customCommandDetail, customCommandSource, invokedCustomCommand, matchingCustomCommands, slashMenu } from './composerCommands';
import type { ComposerSkill } from './composerSkills';

function command(name: string, source: CustomCommand['source'], extra: Partial<CustomCommand> = {}): CustomCommand {
  return { name, description: null, argument_hint: null, source, path: `${name}.md`, ignored: [], ...extra };
}

const COMMANDS: CustomCommand[] = [
  command('standup', 'user'),
  command('review', 'workspace', { description: 'Review the diff', argument_hint: '[files]', ignored: ['allowed-tools', 'model'] }),
  command('frontend:lint', 'workspace'),
  command('rebase', 'user'),
  // A host command's name: never offered.
  command('help', 'workspace'),
];

const SKILLS: ComposerSkill[] = [
  { id: '1', name: 'style' },
  { id: '2', name: 'Review' },
];

const labels = (query: string): string[] =>
  slashMenu(query, COMMANDS, SKILLS).map((entry) => {
    switch (entry.kind) {
      case 'host':
        return `host ${entry.command.name}`;
      case 'custom':
        return `${entry.command.source} /${entry.command.name}`;
      case 'skill':
        return `skill /${entry.skill.name}`;
    }
  });

describe('slashMenu', () => {
  it('orders host commands, then workspace commands, then the person\'s, then skills', () => {
    expect(labels('')).toEqual([
      'host /new',
      'host /plan',
      'host /undo',
      'host /agent',
      'host /clear',
      'host /help',
      'workspace /frontend:lint',
      'workspace /review',
      'user /rebase',
      'user /standup',
      // "Review" is hidden by the command of its name: the message would run the command.
      'skill /style',
    ]);
  });

  it('filters every section by the query, names starting with it first', () => {
    expect(labels('re')).toEqual(['workspace /review', 'user /rebase']);
    expect(labels('lint')).toEqual(['workspace /frontend:lint']);
    expect(labels('st')).toEqual(['user /standup', 'skill /style']);
    expect(labels('he')).toEqual(['host /help']);
  });

  it('lists at most 50 custom commands', () => {
    const many = Array.from({ length: 60 }, (_, i) => command(`c${String(i).padStart(2, '0')}`, 'user'));
    expect(matchingCustomCommands(many, 'c')).toHaveLength(50);
  });
});

describe('source badges and details', () => {
  it('names the source and lists what the host ignored', () => {
    const review = COMMANDS[1];
    if (review === undefined) throw new Error('fixture');
    expect(customCommandSource(review)).toBe('workspace');
    expect(customCommandSource(command('x', 'user'))).toBe('yours');
    expect(customCommandDetail(review)).toBe('Review the diff · [files] · ignored: allowed-tools, model');
    expect(customCommandDetail(command('x', 'user'))).toBe('x.md');
  });
});

describe('invokedCustomCommand', () => {
  it('is the command the first word names, with the rest as its arguments', () => {
    expect(invokedCustomCommand('/review src/a.rs  carefully ', COMMANDS)).toEqual({ name: 'review', arguments: 'src/a.rs  carefully' });
    expect(invokedCustomCommand('  /frontend:lint', COMMANDS)).toEqual({ name: 'frontend:lint', arguments: '' });
    expect(invokedCustomCommand('/review\nall of it', COMMANDS)).toEqual({ name: 'review', arguments: 'all of it' });
  });

  it('is none for a longer word, a later "/", another case or a host command\'s name', () => {
    expect(invokedCustomCommand('/reviewer x', COMMANDS)).toBeNull();
    expect(invokedCustomCommand('please /review', COMMANDS)).toBeNull();
    expect(invokedCustomCommand('/Review x', COMMANDS)).toBeNull();
    expect(invokedCustomCommand('/help', COMMANDS)).toBeNull();
    expect(invokedCustomCommand('/review', [])).toBeNull();
  });
});
