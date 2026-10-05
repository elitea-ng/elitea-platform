import { Db, DbClosedError } from '@/db/sql';
import { openNodeSqlite } from '@/test/nodeSqlite';

async function table() {
  const db = new Db(openNodeSqlite());
  await db.exec('CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)');
  return db;
}

describe('Db', () => {
  it('commits a transaction as a whole', async () => {
    const db = await table();
    await db.transaction(async (tx) => {
      await tx.run('INSERT INTO t VALUES (?, ?)', [1, 'a']);
      await tx.run('INSERT INTO t VALUES (?, ?)', [2, 'b']);
    });
    expect(await db.all('SELECT id, v FROM t ORDER BY id')).toEqual([
      { id: 1, v: 'a' },
      { id: 2, v: 'b' },
    ]);
  });

  it('rolls back everything when the work throws part-way (a crash between statements)', async () => {
    const db = await table();
    await expect(
      db.transaction(async (tx) => {
        await tx.run('INSERT INTO t VALUES (?, ?)', [1, 'a']);
        throw new Error('crash');
      }),
    ).rejects.toThrow('crash');
    expect(await db.all('SELECT * FROM t')).toEqual([]);
    // The connection is usable afterwards.
    await db.run('INSERT INTO t VALUES (?, ?)', [3, 'c']);
    expect(await db.get('SELECT v FROM t WHERE id = 3')).toEqual({ v: 'c' });
  });

  it('serialises callers: nothing interleaves with an open transaction', async () => {
    const db = await table();
    const order: string[] = [];
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    const tx = db.transaction(async (t) => {
      order.push('tx:start');
      await t.run('INSERT INTO t VALUES (1, ?)', ['in-tx']);
      await gate;
      order.push('tx:end');
    });
    const outside = db.run('UPDATE t SET v = ? WHERE id = 1', ['outside']).then(() => order.push('outside'));
    await new Promise((r) => setTimeout(r, 5));
    expect(order).toEqual(['tx:start']);
    release();
    await Promise.all([tx, outside]);
    expect(order).toEqual(['tx:start', 'tx:end', 'outside']);
    expect(await db.get('SELECT v FROM t WHERE id = 1')).toEqual({ v: 'outside' });
  });

  it('closes after queued work and refuses later calls', async () => {
    const db = await table();
    const write = db.run('INSERT INTO t VALUES (1, ?)', ['x']);
    await db.close();
    await expect(write).resolves.toEqual({ changes: 1 });
    expect(db.isClosed).toBe(true);
    await expect(db.get('SELECT 1')).rejects.toBeInstanceOf(DbClosedError);
  });
});
