/** Conversations whose name contains the query (case-insensitive); all of them for a blank query. */
export function filterConversations<T extends { name?: string | null }>(rows: readonly T[], query: string): T[] {
  const q = query.trim().toLocaleLowerCase();
  if (q === '') return [...rows];
  return rows.filter((row) => (row.name || 'Untitled conversation').toLocaleLowerCase().includes(q));
}
