import { deleteExpoDatabase, openExpoConnection } from '@/db/expoDriver';
import { migrate } from '@/db/migrations';
import { keyConnection } from '@/db/open';
import { upsertConversationRow, upsertMessageRow } from '@/db/repo';
import { searchConversations, type SearchHit } from '@/db/search';
import { Db } from '@/db/sql';
import { SAMPLE_CHATS, SAMPLE_PROJECT_ID, sampleChatRow } from '@/features/designGallery/fixtures';
import { log } from '@/lib/log';

/**
 * Development builds only: the Chats preview's search runs the REAL offline search on the
 * device: a throwaway SQLCipher database (its own file and a fixed preview key, sample data
 * only, never the workspace store), migrated and seeded through the same repo writes the sync
 * engine uses, then asked through `searchConversations`. So a preview screenshot of search
 * results is also the on-device proof that this build's SQLite has both SQLCipher and FTS5.
 */

const FILE = 'design-preview-search.db';
const PREVIEW_KEY = 'a1'.repeat(32);

let ready: Promise<{ db: Db; cipher: string | null; fts5: boolean }> | null = null;

async function open(now: number) {
  await deleteExpoDatabase(FILE).catch(() => {});
  const raw = await openExpoConnection(FILE);
  await keyConnection(raw, PREVIEW_KEY, { requireCipher: true });
  const db = new Db(raw);
  await migrate(db);
  const cipher = (await db.get<{ cipher_version?: string }>('PRAGMA cipher_version;'))?.cipher_version ?? null;
  const fts5 = (await db.get<{ on: number }>("SELECT sqlite_compileoption_used('ENABLE_FTS5') AS on"))?.on === 1;
  await db.transaction(async (tx) => {
    for (const chat of SAMPLE_CHATS) {
      const row = sampleChatRow(chat, now);
      await upsertConversationRow(tx, SAMPLE_PROJECT_ID, { ...row, created_at: row.updated_at, duration: -1, message_groups_count: chat.messages?.length ?? 0 }, 1);
      for (const [i, content] of (chat.messages ?? []).entries()) {
        await upsertMessageRow(
          tx,
          SAMPLE_PROJECT_ID,
          chat.id,
          { id: String(chat.id * 100 + i + 1), uid: `preview-${chat.id}-${i}`, conversation_id: String(chat.id), role: 'assistant', content, created_at: row.updated_at },
          1,
        );
      }
    }
  });
  log.info('design preview search store', { cipher, fts5 });
  return { db, cipher, fts5 };
}

/** Searches the preview store (opened and seeded on first use). */
export async function previewSearch(query: string, now: number): Promise<{ hits: SearchHit[]; cipher: string | null; fts5: boolean }> {
  ready ??= open(now);
  try {
    const store = await ready;
    return { hits: await searchConversations(store.db, SAMPLE_PROJECT_ID, query), cipher: store.cipher, fts5: store.fts5 };
  } catch (error) {
    ready = null;
    throw error;
  }
}
