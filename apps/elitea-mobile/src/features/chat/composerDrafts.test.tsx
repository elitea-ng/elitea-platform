import { act, fireEvent, render, screen, waitFor } from '@testing-library/react-native';

import { migrate } from '@/db/migrations';
import { Db } from '@/db/sql';
import { Composer } from '@/features/chat/Composer';
import { composerDrafts, DraftStore, draftKey } from '@/features/chat/composerDrafts';
import { openNodeSqlite } from '@/test/nodeSqlite';

async function openDb(): Promise<Db> {
  const db = new Db(openNodeSqlite());
  await migrate(db);
  return db;
}

const stored = (db: Db) => db.all<{ key: string; text: string }>('SELECT key, text FROM drafts ORDER BY key');

describe('draftKey', () => {
  it('is the project and the conversation, or "new" before the conversation exists', () => {
    expect(draftKey(7, '42')).toBe('7:42');
    expect(draftKey(7, null)).toBe('7:new');
  });
});

describe('DraftStore', () => {
  it('keeps one draft per key and drops an empty one', () => {
    const store = new DraftStore({ db: () => null });
    store.set('7:1', 'for A');
    store.set('7:2', 'for B');
    store.set(draftKey(7, null), 'not created yet');
    expect(store.get('7:1')).toBe('for A');
    expect(store.get('7:2')).toBe('for B');
    expect(store.get('7:new')).toBe('not created yet');
    store.set('7:1', '   ');
    expect(store.get('7:1')).toBeNull();
  });

  it('clears the "new" draft once the first send created the conversation', () => {
    const store = new DraftStore({ db: () => null });
    store.set(draftKey(7, null), 'hello');
    store.clearIfSent(draftKey(7, null), (text) => text === 'hello');
    expect(store.get('7:new')).toBeNull();
  });

  it('keeps an edit made after the send', () => {
    const store = new DraftStore({ db: () => null });
    store.set('7:1', 'hello and more');
    store.clearIfSent('7:1', (text) => text === 'hello');
    expect(store.get('7:1')).toBe('hello and more');
  });

  it('keeps only the most recently changed drafts', () => {
    let now = 0;
    const store = new DraftStore({ db: () => null, now: () => ++now, max: 3 });
    for (const id of ['1', '2', '3', '4']) store.set(draftKey(7, id), `text ${id}`);
    expect(store.get('7:1')).toBeNull();
    expect(['7:2', '7:3', '7:4'].map((k) => store.get(k))).toEqual(['text 2', 'text 3', 'text 4']);
  });

  it('writes after the delay, caps the table, and loads the drafts back on the next open', async () => {
    const db = await openDb();
    let now = 0;
    const store = new DraftStore({ db: () => db, now: () => ++now, max: 2, delayMs: 20 });
    await store.flush();
    store.set('7:1', 'one');
    store.set('7:2', 'two');
    store.set('7:3', 'three');
    expect(await stored(db)).toEqual([]);
    await waitFor(async () =>
      expect(await stored(db)).toEqual([
        { key: '7:2', text: 'two' },
        { key: '7:3', text: 'three' },
      ]),
    );
    store.set('7:2', '');
    await store.flush();
    expect(await stored(db)).toEqual([{ key: '7:3', text: 'three' }]);

    // A relaunch: a new store over the same file.
    const relaunched = new DraftStore({ db: () => db });
    await relaunched.flush();
    expect(relaunched.get('7:3')).toBe('three');
    expect(relaunched.loads).toBe(1);
  });

  it('keeps changes made while the database is closed and writes them when it opens', async () => {
    const db = await openDb();
    let open: Db | null = null;
    const listeners = new Set<() => void>();
    const store = new DraftStore({
      db: () => open,
      subscribe: (l) => {
        listeners.add(l);
        return () => listeners.delete(l);
      },
    });
    store.set('7:1', 'typed while locked');
    await store.flush();
    open = db;
    for (const l of listeners) l();
    await store.flush();
    expect(await stored(db)).toEqual([{ key: '7:1', text: 'typed while locked' }]);
  });

  it('forgets everything on clear (the session wipe)', () => {
    const store = new DraftStore({ db: () => null });
    store.set('7:1', 'secret');
    store.clear();
    expect(store.get('7:1')).toBeNull();
  });
});

describe('Composer drafts', () => {
  beforeEach(() => composerDrafts.clear());

  it('brings the unsent text back after switching to another conversation and back', async () => {
    const onSend = jest.fn(async () => null);
    const { rerender } = await render(<Composer busy={false} onSend={onSend} unsentKey={draftKey(7, '1')} />);
    await fireEvent.changeText(screen.getByLabelText('Message'), 'half a thought');
    await rerender(<Composer busy={false} onSend={onSend} unsentKey={draftKey(7, '2')} />);
    expect(screen.getByLabelText('Message').props.value).toBe('');
    await fireEvent.changeText(screen.getByLabelText('Message'), 'for B');
    await rerender(<Composer busy={false} onSend={onSend} unsentKey={draftKey(7, '1')} />);
    expect(screen.getByLabelText('Message').props.value).toBe('half a thought');
  });

  it('restores the draft when the thread is opened again', async () => {
    const first = await render(<Composer busy={false} onSend={jest.fn(() => null)} unsentKey="7:1" />);
    await fireEvent.changeText(screen.getByLabelText('Message'), 'left the screen');
    await first.unmount();
    await render(<Composer busy={false} onSend={jest.fn(() => null)} unsentKey="7:1" />);
    expect(screen.getByLabelText('Message').props.value).toBe('left the screen');
  });

  it('clears the draft on an accepted send, keeps it on a failed one', async () => {
    const failing = jest.fn(async () => 'This message could not be sent.');
    const view = await render(<Composer busy={false} onSend={failing} unsentKey="7:1" />);
    await fireEvent.changeText(screen.getByLabelText('Message'), 'hello');
    await fireEvent.press(screen.getByLabelText('Send'));
    await waitFor(() => expect(screen.getByRole('alert')).toBeTruthy());
    expect(composerDrafts.get('7:1')).toBe('hello');

    await view.rerender(<Composer busy={false} onSend={jest.fn(async () => null)} unsentKey="7:1" />);
    await fireEvent.press(screen.getByLabelText('Send'));
    await waitFor(() => expect(screen.getByLabelText('Message').props.value).toBe(''));
    expect(composerDrafts.get('7:1')).toBeNull();
  });

  it('clears the draft when the send is accepted after the thread was left', async () => {
    let accept: (problem: string | null) => void = () => undefined;
    const onSend = jest.fn(() => new Promise<string | null>((resolve) => (accept = resolve)));
    const view = await render(<Composer busy={false} onSend={onSend} unsentKey="7:1" />);
    await fireEvent.changeText(screen.getByLabelText('Message'), 'sent then left');
    await fireEvent.press(screen.getByLabelText('Send'));
    await view.unmount();
    await act(async () => accept(null));
    expect(composerDrafts.get('7:1')).toBeNull();
  });
});
