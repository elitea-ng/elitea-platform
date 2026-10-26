/** The one query key both the settings panel and the proposal card read `mcp_list` under. */
export const mcpListKey = (workspaceId: string): readonly unknown[] => ['workspace', 'mcp', workspaceId];
