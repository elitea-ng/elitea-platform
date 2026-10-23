import { forkableKeys, type ForkPointState } from '@/features/chat/forkPoints';

// "Fork from here" is decided per message (client contract 1.11): only the exchange in flight
// is held back, never the settled ones before it.

const m = (key: string, role: 'user' | 'assistant', text: string, extra: { uuid?: string | null; isStreaming?: boolean } = {}) => ({
  key,
  role,
  text,
  uuid: extra.uuid === undefined ? key : extra.uuid,
  isStreaming: extra.isStreaming ?? false,
});
const idle: ForkPointState = { liveActive: false, liveContinuation: false, liveUserText: null, regeneratingUuid: null, paused: false };
const thread = [m('q1', 'user', 'first'), m('a1', 'assistant', 'one'), m('q2', 'user', 'second'), m('a2', 'assistant', 'two')];
const keys = (set: Set<string>) => [...set];

describe('forkableKeys', () => {
  it('idle: every persisted message', () => {
    expect(keys(forkableKeys(thread, idle))).toEqual(['q1', 'a1', 'q2', 'a2']);
    expect(keys(forkableKeys([...thread, m('q3', 'user', 'unsent', { uuid: null })], idle))).toEqual(['q1', 'a1', 'q2', 'a2']);
  });

  it('a turn streaming whose question is not in the transcript yet holds nothing back', () => {
    expect(keys(forkableKeys(thread, { ...idle, liveActive: true, liveUserText: 'third' }))).toEqual(['q1', 'a1', 'q2', 'a2']);
  });

  it('a turn streaming whose question is persisted holds back that exchange only', () => {
    const live = [...thread, m('q3', 'user', 'third')];
    expect(keys(forkableKeys(live, { ...idle, liveActive: true, liveUserText: 'third' }))).toEqual(['q1', 'a1', 'q2', 'a2']);
    const withAnswer = [...live, m('a3', 'assistant', '', { isStreaming: true })];
    expect(keys(forkableKeys(withAnswer, { ...idle, liveActive: true, liveUserText: 'third' }))).toEqual(['q1', 'a1', 'q2', 'a2']);
  });

  it('an answer another device still streams holds back its exchange', () => {
    const streaming = [...thread.slice(0, 3), m('a2', 'assistant', 'tw', { isStreaming: true })];
    expect(keys(forkableKeys(streaming, idle))).toEqual(['q1', 'a1']);
  });

  it('an answer waiting for the user (approval) or being continued holds back its exchange', () => {
    expect(keys(forkableKeys(thread, { ...idle, paused: true }))).toEqual(['q1', 'a1']);
    expect(keys(forkableKeys(thread, { ...idle, liveActive: true, liveContinuation: true }))).toEqual(['q1', 'a1']);
  });

  it('a regeneration holds back the rewritten answer and its question', () => {
    expect(keys(forkableKeys(thread, { ...idle, liveActive: true, liveUserText: 'second', regeneratingUuid: 'a2' }))).toEqual(['q1', 'a1']);
  });
});
