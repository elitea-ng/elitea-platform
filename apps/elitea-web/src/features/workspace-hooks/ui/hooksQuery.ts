/** The one query key both the settings panel and the proposal card read `hooks_list` under. */
export const hooksListKey = (workspaceId: string): readonly unknown[] => ['workspace', 'hooks', workspaceId];
