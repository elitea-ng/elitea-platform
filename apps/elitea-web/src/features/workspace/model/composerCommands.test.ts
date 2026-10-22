import { describe, expect, it } from 'vitest';

import type { CustomCommand } from '@/shared/desktop/workspaceIpc';

import { customCommandDetail, customCommandSource, invokedCustomCommand, invokedPick, matchingCustomCommands, slashMenu } from './composerCommands';
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
  command('model', 'user'),
  command('compact', 'workspace'),
];

const SKILLS: ComposerSkill[] = [
  { id: '1', name: 'style' },
  { id: '2', name: 'Review' },
  { id: '9', name: 'deploy', project: true },
  { id: '8', name: 'audit', project: true },
];

const labels = (query: string): string[] =>
  slashMenu(query, COMMANDS, SKILLS).map((entry) => {
    switch (entry.kind) {
      case 'host':
        return `host ${entry.command.name}`;
      case 'custom':
        return `${entry.command.source} /${entry.command.name}`;
      case 'skill':
        return `${entry.skill.project === true ? 'project' : 'skill'} /${entry.skill.name}`;
    }
  });

describe('slashMenu', () => {
  it('orders host commands, then workspace commands, then the person\'s, then the version\'s skills, then the project\'s', () => {
    expect(labels('')).toEqual([
      'host /new',
      'host /plan',
      'host /undo',
      'host /agent',
      'host /model',
      'host /clear',
      'host /compact',
      'host /help',
      'workspace /frontend:lint',
      'workspace /review',
      'user /rebase',
      'user /standup',
      // "Review" is hidden by the command of its name: the message would run the command.
      'skill /style',
      // The project's skills after the version's.
      'project /audit',
      'project /deploy',
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

  it('matches in any case, as the menu filters', () => {
    expect(invokedCustomCommand('/Review x', COMMANDS)).toEqual({ name: 'review', arguments: 'x' });
    expect(invokedCustomCommand('/FRONTEND:Lint', COMMANDS)).toEqual({ name: 'frontend:lint', arguments: '' });
  });

  it('is none for a longer word, a later "/" or a host command\'s name', () => {
    expect(invokedCustomCommand('/reviewer x', COMMANDS)).toBeNull();
    expect(invokedCustomCommand('please /review', COMMANDS)).toBeNull();
    expect(invokedCustomCommand('/help', COMMANDS)).toBeNull();
    expect(invokedCustomCommand('/model gpt-5', COMMANDS)).toBeNull();
    expect(invokedCustomCommand('/compact', COMMANDS)).toBeNull();
    expect(invokedCustomCommand('/review', [])).toBeNull();
  });
});

describe('invokedPick', () => {
  const skill = (name: string, extra: Partial<ComposerSkill> = {}): ComposerSkill => ({ id: name, name, ...extra });
  const SKILLS = [skill('review code'), skill('Review'), skill('Deploy', { project: true })];

  it('applies a skill whose name is longer than the custom command it starts with', () => {
    // Picking "review code" from the menu inserts `/review code `.
    expect(invokedPick('/review code src/a.rs', COMMANDS, SKILLS)).toEqual({ command: null, skill: SKILLS[0] });
    expect(invokedPick('/Review Code', COMMANDS, SKILLS)).toEqual({ command: null, skill: SKILLS[0] });
    // `/review` alone (or with other words) is still the command.
    expect(invokedPick('/review src/a.rs', COMMANDS, SKILLS)).toEqual({ command: { name: 'review', arguments: 'src/a.rs' }, skill: null });
  });

  it('lets a custom command shadow a skill of its name in any case, typed by hand too', () => {
    expect(invokedPick('/REVIEW now', COMMANDS, SKILLS)).toEqual({ command: { name: 'review', arguments: 'now' }, skill: null });
    expect(slashMenu('review', COMMANDS, SKILLS).flatMap((entry) => (entry.kind === 'skill' ? [entry.skill.name] : []))).toEqual(['review code']);
  });

  it('is a skill when no command matches, and neither for plain text', () => {
    expect(invokedPick('/deploy it', COMMANDS, SKILLS)).toEqual({ command: null, skill: SKILLS[2] });
    expect(invokedPick('/Review it', [], SKILLS)).toEqual({ command: null, skill: SKILLS[1] });
    expect(invokedPick('hello', COMMANDS, SKILLS)).toEqual({ command: null, skill: null });
  });
});
