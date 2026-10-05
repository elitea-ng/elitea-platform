import {
  EVERYONE,
  deriveMentions,
  detectTrigger,
  findMentions,
  highlightSpans,
  replaceRange,
  type TriggerChar,
} from '@/features/chat/mentions/mentions';

const ALL: TriggerChar[] = ['@', '#', '~', '/'];
const at = (text: string, caret = text.length) => detectTrigger(text, { start: caret, end: caret }, ALL);

const people = [
  { userId: 9, name: 'Ann' },
  { userId: 42, name: 'Anna' },
  { userId: 7, name: 'Bob Stone' },
  { userId: 11, name: '李雷' },
  { userId: 12, name: 'Zoë 🚀' },
];

describe('detectTrigger', () => {
  it('opens at the start of the text or after whitespace, up to the caret', () => {
    expect(at('@an')).toEqual({ trigger: '@', query: 'an', start: 0, end: 3 });
    expect(at('hi #Res')).toEqual({ trigger: '#', query: 'Res', start: 3, end: 7 });
    expect(at('use ~sum')).toEqual({ trigger: '~', query: 'sum', start: 4, end: 8 });
    expect(at('x\n@')).toEqual({ trigger: '@', query: '', start: 2, end: 3 });
  });

  it('stays closed inside a word, after whitespace, and for a range selection', () => {
    expect(at('mail@example')).toBeNull();
    expect(at('@ann ')).toBeNull();
    expect(at('hello')).toBeNull();
    expect(detectTrigger('@an', { start: 0, end: 3 }, ALL)).toBeNull();
  });

  it('reads the token the caret is in, not the end of the text', () => {
    expect(at('@an and more', 3)).toEqual({ trigger: '@', query: 'an', start: 0, end: 3 });
  });

  it('keeps one inner slash for the two-phase tool picker, and ignores URLs', () => {
    expect(at('/github')).toEqual({ trigger: '/', query: 'github', start: 0, end: 7 });
    expect(at('/github/li')).toEqual({ trigger: '/', query: 'github/li', start: 0, end: 10 });
    expect(at('/a/b/c')).toBeNull();
    expect(at('see http://x.io/a')).toBeNull();
  });

  it('only opens the triggers it is given', () => {
    expect(detectTrigger('#x', { start: 2, end: 2 }, ['@'])).toBeNull();
  });
});

describe('replaceRange', () => {
  it('replaces the token and puts the caret after the insert', () => {
    expect(replaceRange('hi @an!', 3, 6, '@Anna ')).toEqual({ text: 'hi @Anna !', caret: 9 });
    expect(replaceRange('#res', 0, 4, '')).toEqual({ text: '', caret: 0 });
  });
});

describe('deriveMentions (the web rule)', () => {
  it('prefers the longest name: "@Anna" is Anna even when Ann exists', () => {
    expect(deriveMentions('hi @Anna', people)).toEqual({ userIds: [42], everyone: false });
    expect(deriveMentions('hi @Ann', people)).toEqual({ userIds: [9], everyone: false });
  });

  it('needs a word boundary after the name', () => {
    expect(deriveMentions('@Annabel hi', people).userIds).toEqual([]);
    for (const end of ['.', ',', '!', '?', ';', ':', '\n', ' ', '']) {
      expect(deriveMentions(`@Anna${end}`, people).userIds).toEqual([42]);
    }
  });

  it('is case-insensitive and reads multi-word, CJK and emoji names', () => {
    expect(deriveMentions('@bob stone please', people).userIds).toEqual([7]);
    expect(deriveMentions('@李雷 你好', people).userIds).toEqual([11]);
    expect(deriveMentions('cc @Zoë 🚀!', people).userIds).toEqual([12]);
  });

  it('dedupes and keeps the order of first mention', () => {
    expect(deriveMentions('@Anna @Ann @Anna', people).userIds).toEqual([42, 9]);
  });

  it('reads @Everyone, unless the conversation does not offer it', () => {
    expect(deriveMentions(`@${EVERYONE} look`, people)).toEqual({ userIds: [], everyone: true });
    expect(deriveMentions('@everyone', people, { everyone: false }).everyone).toBe(false);
  });

  it('follows edits after a pick: a deleted name is no mention, a typed one is', () => {
    expect(deriveMentions('hi Anna ', people).userIds).toEqual([]);
    expect(deriveMentions('hi @Bob Stone and @Ann', people).userIds).toEqual([7, 9]);
    expect(deriveMentions('mail ann@Anna.io', people).userIds).toEqual([42]);
  });

  it('never matches an empty or blank name', () => {
    expect(findMentions('@ x', [{ userId: 1, name: ' ' }], { everyone: false })).toEqual([]);
  });
});

describe('highlightSpans', () => {
  it('tints people, agents, skills and tool tokens, and leaves the rest plain', () => {
    expect(
      highlightSpans('@Anna ask #Research Agent with ~summarize and /github/list_issues now', {
        people: ['Anna'],
        agents: ['Research Agent'],
      }),
    ).toEqual([
      { kind: 'mention', text: '@Anna' },
      { kind: 'text', text: ' ask ' },
      { kind: 'agent', text: '#Research Agent' },
      { kind: 'text', text: ' with ' },
      { kind: 'skill', text: '~summarize' },
      { kind: 'text', text: ' and ' },
      { kind: 'tool', text: '/github/list_issues' },
      { kind: 'text', text: ' now' },
    ]);
  });

  it('leaves unknown names, mid-word sigils and bare slashes alone', () => {
    expect(highlightSpans('@Nobody a#b /tmp and #Unknown', { people: ['Anna'], agents: ['Research'] })).toEqual([
      { kind: 'text', text: '@Nobody a#b /tmp and #Unknown' },
    ]);
    expect(highlightSpans('')).toEqual([]);
  });
});
