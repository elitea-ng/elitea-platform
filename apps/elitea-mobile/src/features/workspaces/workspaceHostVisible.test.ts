/**
 * F1: "every signed-in header shows the active workspace's host" (docs/design.md). On iOS a
 * large title hides the inline WorkspaceTitle (host + mark) until it collapses, and it collapses
 * only when the screen's root is a scroll view adjusting its content inset. So every tab root
 * with a large title must (1) be rooted in such a scroll view and (2) show the host while the
 * title is expanded: WorkspacePill, or (Settings) the host as its first row.
 */
declare const __dirname: string;
const fs = require('fs') as { readFileSync(path: string, encoding: 'utf8'): string; readdirSync(path: string): string[]; existsSync(path: string): boolean };
const path = require('path') as { join(...parts: string[]): string };

const TABS = path.join(__dirname, '../../app/(app)/(tabs)');
/** Tab roots that show the host in their own content instead of the pill: the file and the row. */
const HOST_IN_CONTENT: Record<string, { file: string; row: RegExp; mounted: RegExp }> = {
  settings: {
    file: path.join(__dirname, '../settings/SettingsList.tsx'),
    row: /<ListRow\s+title=\{serverOrigin \? workspaceAddressTitle\(serverOrigin\) : 'Not connected'\}/,
    mounted: /<SettingsList\b/,
  },
};

const roots = fs
  .readdirSync(TABS)
  .filter((dir) => fs.existsSync(path.join(TABS, dir, '_layout.tsx')))
  .filter((dir) => fs.readFileSync(path.join(TABS, dir, '_layout.tsx'), 'utf8').includes('largeTitleOptions('));

describe('tab roots with a large title keep the workspace host on screen (F1)', () => {
  it('finds the large-title tab roots', () => {
    expect(roots).toEqual(expect.arrayContaining(['(chats)', 'discover', 'settings']));
  });

  it.each(roots)('%s is rooted in an inset-adjusting scroll view and shows the host', (dir) => {
    const source = fs.readFileSync(path.join(TABS, dir, 'index.tsx'), 'utf8');
    expect(source).toMatch(/contentInsetAdjustmentBehavior="automatic"/);
    const content = HOST_IN_CONTENT[dir];
    if (content) {
      expect(source).toMatch(content.mounted);
      expect(fs.readFileSync(content.file, 'utf8')).toMatch(content.row);
    } else {
      expect(source).toMatch(/<WorkspacePill\s*\/>/);
    }
  });
});
