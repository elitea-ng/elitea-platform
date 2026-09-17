import { within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { AgentPipelineVersionOption } from '../lib/types';

import { AgentPipelineVersionSelector } from './AgentPipelineVersionSelector';

// Noon UTC so a local-timezone test runner never crosses a day boundary and
// renders a different calendar date than the UTC one written here.
const versions: readonly AgentPipelineVersionOption[] = [
  { id: 1, name: 'base', created_at: '2026-01-01T12:00:00Z' },
  { id: 2, name: 'v1', created_at: '2026-02-01T12:00:00Z' },
  { id: 3, name: 'v0', created_at: '2026-01-15T12:00:00Z' },
];

/** Mirrors `formatVersionDisplayText`'s date formatting exactly, so assertions aren't hard-coded against one timezone. */
function formatDate(iso: string): string {
  const date = new Date(iso);
  const day = String(date.getDate()).padStart(2, '0');
  const month = String(date.getMonth() + 1).padStart(2, '0');
  return `${day}.${month}.${date.getFullYear()}`;
}

describe('AgentPipelineVersionSelector', () => {
  it('shows the latest ("base") version label when the selected version is the latest', () => {
    const { getByText } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={1}
        versions={versions}
        onSelectVersion={vi.fn()}
      />,
    );
    expect(getByText('base')).toBeInTheDocument();
  });

  it('shows the formatted name + date for a non-latest selected version', () => {
    const { getByText } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={2}
        versions={versions}
        onSelectVersion={vi.fn()}
      />,
    );
    expect(getByText(`v1 – ${formatDate('2026-02-01T12:00:00Z')}`)).toBeInTheDocument();
  });

  it('shows "Invalid version" and a warning icon when applicationVersionId does not match any version', () => {
    const { getByText, container } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={999}
        versions={versions}
        onSelectVersion={vi.fn()}
      />,
    );
    expect(getByText('Invalid version')).toBeInTheDocument();
    expect(container.querySelector('svg[data-testid="WarningAmberIcon"]')).toBeInTheDocument();
  });

  it('falls back to the latest version display when applicationVersionId is undefined', () => {
    const { getByText } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={undefined}
        versions={versions}
        onSelectVersion={vi.fn()}
      />,
    );
    expect(getByText('base')).toBeInTheDocument();
  });

  it('opens the menu, lists versions with the latest first then newest-first by date, and calls onSelectVersion on click', async () => {
    const user = userEvent.setup();
    const onSelectVersion = vi.fn();
    const { getByTestId, getAllByRole, getByText } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={1}
        versions={versions}
        onSelectVersion={onSelectVersion}
      />,
    );
    await user.click(getByTestId('version-selector-trigger'));

    // Issue 940/A11 (ELITEA-3279) — each row's `textContent` now also carries
    // a secondary "creator · timestamp" line when the version has one (none
    // of this fixture's rows carry an `author`, so only the timestamp half
    // shows — see `versionMetaLine`'s own doc comment). `.toContain` rather
    // than an exact match keeps this assertion about ORDER and the PRIMARY
    // label, not byte-exact text content.
    const items = getAllByRole('menuitem').map((el) => el.textContent);
    expect(items[0]).toContain('base');
    expect(items[1]).toContain(`v1 – ${formatDate('2026-02-01T12:00:00Z')}`);
    expect(items[2]).toContain(`v0 – ${formatDate('2026-01-15T12:00:00Z')}`);

    await user.click(getByText(`v1 – ${formatDate('2026-02-01T12:00:00Z')}`));
    expect(onSelectVersion).toHaveBeenCalledWith(expect.objectContaining({ id: 2, name: 'v1' }));
  });

  /**
   * Issue 940/A11 (ELITEA-3278/3280) — the search box filters by name AND
   * creator, preserving the newest-first sort order.
   */
  describe('search (issue 940/A11)', () => {
    const versionsWithAuthors: readonly AgentPipelineVersionOption[] = [
      { id: 1, name: 'base', created_at: '2026-01-01T12:00:00Z' },
      { id: 2, name: 'Production-v1', created_at: '2026-02-01T12:00:00Z', author: { name: 'Alice Smith', email: 'alice@example.com' } },
      { id: 3, name: 'staging-v2', created_at: '2026-01-15T12:00:00Z', author: { name: 'Bob Jones', email: 'bob@example.com' } },
      { id: 4, name: 'dev-test', created_at: '2026-01-20T12:00:00Z', author: { name: 'Alice Johnson', email: 'alice.j@example.com' } },
    ];

    async function openMenu(user: ReturnType<typeof userEvent.setup>, versionsToRender: readonly AgentPipelineVersionOption[] = versionsWithAuthors) {
      const utils = renderWithTheme(
        <AgentPipelineVersionSelector
          applicationVersionId={1}
          versions={versionsToRender}
          onSelectVersion={vi.fn()}
        />,
      );
      await user.click(utils.getByTestId('version-selector-trigger'));
      return utils;
    }

    /** No `onSetDefaultVersion`/`onDeleteVersion` is passed by `openMenu` above, so every `role="menuitem"` row here is a VERSION row — neither command item renders at all (see the "renders no set-default item at all" test above). */
    function rowNames(getAllByRole: ReturnType<typeof renderWithTheme>['getAllByRole']): string[] {
      return getAllByRole('menuitem').map((el) => el.textContent ?? '');
    }

    it('filters by version name, case-insensitively and partially, preserving newest-first order', async () => {
      const user = userEvent.setup();
      const { getAllByRole, getByTestId } = await openMenu(user);

      const search = getByTestId('version-selector-search');
      await user.type(search, 'PROD');

      const names = rowNames(getAllByRole);
      expect(names).toHaveLength(1);
      expect(names[0]).toContain('Production-v1');
    });

    it('filters by creator name', async () => {
      const user = userEvent.setup();
      const { getAllByRole, getByTestId } = await openMenu(user);

      const search = getByTestId('version-selector-search');
      await user.type(search, 'alice');

      const names = rowNames(getAllByRole);
      expect(names).toHaveLength(2);
      // Newest first (2026-02-01 before 2026-01-20), same order the caller's
      // own timestamp sort already established before this search ran.
      expect(names[0]).toContain('Production-v1');
      expect(names[1]).toContain('dev-test');
    });

    it('filters by creator email', async () => {
      const user = userEvent.setup();
      const { getAllByRole, getByTestId } = await openMenu(user);

      const search = getByTestId('version-selector-search');
      await user.type(search, 'bob@example.com');

      const names = rowNames(getAllByRole);
      expect(names).toHaveLength(1);
      expect(names[0]).toContain('staging-v2');
    });

    it('shows "No versions found" for a query that matches nothing, and restores the full list on clear', async () => {
      const user = userEvent.setup();
      const { getByTestId, getByText, queryByText, getByRole } = await openMenu(user);

      const search = getByTestId('version-selector-search');
      await user.type(search, 'nonexistent-xyz-123');
      expect(getByText('No versions found')).toBeInTheDocument();

      await user.clear(search);
      expect(queryByText('No versions found')).not.toBeInTheDocument();
      // `within` the menu: "base" also renders on the closed TRIGGER (the
      // selected version's own label), so an unscoped `getByText('base')`
      // finds two matches.
      expect(within(getByRole('menu')).getByText('base')).toBeInTheDocument();
    });

    it('does not show the search box at all when there are no versions', async () => {
      const user = userEvent.setup();
      const { queryByTestId, getByText } = await openMenu(user, []);
      expect(getByText('No versions available')).toBeInTheDocument();
      expect(queryByTestId('version-selector-search')).not.toBeInTheDocument();
    });
  });

  it('does not open the dropdown when disabled', async () => {
    const user = userEvent.setup();
    const { getByTestId, queryByRole } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={1}
        versions={versions}
        disabled
        onSelectVersion={vi.fn()}
      />,
    );
    await user.click(getByTestId('version-selector-trigger'));
    expect(queryByRole('menu')).not.toBeInTheDocument();
  });

  it('shows a switching spinner and makes the trigger inert while a version switch is in flight', async () => {
    const user = userEvent.setup();
    const { getByTestId, queryByRole } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={1}
        versions={versions}
        isSwitchingVersion
        onSelectVersion={vi.fn()}
      />,
    );
    expect(getByTestId('version-selector-switching')).toBeInTheDocument();

    await user.click(getByTestId('version-selector-trigger'));
    expect(queryByRole('menu')).not.toBeInTheDocument();
  });

  it('gives the version list its own scrollable region so overflowing versions stay reachable', async () => {
    const user = userEvent.setup();
    const manyVersions: readonly AgentPipelineVersionOption[] = Array.from({ length: 20 }, (_, i) => ({
      id: i + 1,
      name: `v${i}`,
      created_at: `2026-01-${String((i % 28) + 1).padStart(2, '0')}T12:00:00Z`,
    }));
    const { getByTestId, getByRole } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={1}
        versions={manyVersions}
        onSelectVersion={vi.fn()}
      />,
    );
    await user.click(getByTestId('version-selector-trigger'));

    // The menu's own list (not just its Paper) needs a bounded, scrollable
    // region — otherwise versions beyond the Paper's maxHeight are clipped
    // by the Paper's `overflow: hidden` with no way to reach them.
    const menu = getByRole('menu');
    const computed = getComputedStyle(menu);
    expect(computed.overflowY).toBe('auto');
    expect(computed.maxHeight).toBe('11.5rem');
  });

  it('calls onRefreshVersions from the menu refresh button', async () => {
    const user = userEvent.setup();
    const onRefreshVersions = vi.fn();
    const { getByTestId, getByRole } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={1}
        versions={versions}
        onSelectVersion={vi.fn()}
        onRefreshVersions={onRefreshVersions}
      />,
    );
    await user.click(getByTestId('version-selector-trigger'));
    await user.click(getByRole('button', { name: 'Refresh versions' }));
    expect(onRefreshVersions).toHaveBeenCalledTimes(1);
  });
  /*
   * #147 — the set-default affordance. Two behaviours matter here and neither
   * is "the item renders": that it stays ABSENT for every caller that does not
   * ask for it (the tool card in `ToolCardBody` is one), and that the
   * baseline's own eligibility rule still decides when it may be used.
   */
  it('renders no set-default item at all when the caller supplies no handler', async () => {
    const user = userEvent.setup();
    const { getByTestId, queryByTestId } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={2}
        versions={versions}
        onSelectVersion={vi.fn()}
      />,
    );
    await user.click(getByTestId('version-selector-trigger'));
    expect(queryByTestId('agent-version-set-default')).not.toBeInTheDocument();
    expect(queryByTestId('agent-version-default-marker')).not.toBeInTheDocument();
  });

  it('reports the SELECTED version to onSetDefaultVersion and closes the menu behind it', async () => {
    const user = userEvent.setup();
    const onSetDefaultVersion = vi.fn();
    const { getByTestId, queryByRole } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={2}
        versions={versions}
        onSelectVersion={vi.fn()}
        onSetDefaultVersion={onSetDefaultVersion}
      />,
    );
    await user.click(getByTestId('version-selector-trigger'));
    await user.click(getByTestId('agent-version-set-default'));

    expect(onSetDefaultVersion).toHaveBeenCalledTimes(1);
    expect(onSetDefaultVersion.mock.calls[0]?.[0]).toMatchObject({ id: 2, name: 'v1' });
    // The caller answers with a modal; a menu left open behind it stacks a
    // second focus trap.
    expect(queryByRole('menu')).not.toBeInTheDocument();
  });

  it('marks the default version and refuses to re-pin it — entities/version isSetDefaultDisabled', async () => {
    const user = userEvent.setup();
    const { getByTestId } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={2}
        versions={versions}
        defaultVersionId={2}
        onSelectVersion={vi.fn()}
        onSetDefaultVersion={vi.fn()}
      />,
    );
    await user.click(getByTestId('version-selector-trigger'));

    expect(getByTestId('agent-version-default-marker')).toBeInTheDocument();
    expect(getByTestId('agent-version-set-default')).toHaveAttribute('aria-disabled', 'true');
  });

  it('refuses to pin "base" while no default is recorded, and refuses a published version', async () => {
    const user = userEvent.setup();
    const base = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={1}
        versions={versions}
        onSelectVersion={vi.fn()}
        onSetDefaultVersion={vi.fn()}
      />,
    );
    await user.click(base.getByTestId('version-selector-trigger'));
    expect(base.getByTestId('agent-version-set-default')).toHaveAttribute('aria-disabled', 'true');
    base.unmount();

    const publishedVersions: readonly AgentPipelineVersionOption[] = [
      { id: 1, name: 'base', created_at: '2026-01-01T12:00:00Z' },
      { id: 2, name: 'v1', created_at: '2026-02-01T12:00:00Z', status: 'published' },
    ];
    const published = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={2}
        versions={publishedVersions}
        onSelectVersion={vi.fn()}
        onSetDefaultVersion={vi.fn()}
      />,
    );
    await user.click(published.getByTestId('version-selector-trigger'));
    expect(published.getByTestId('agent-version-set-default')).toHaveAttribute('aria-disabled', 'true');
  });
});

/**
 * #147's delete item. The menu carried version rows and, since #610, one
 * "Set as default" command. The other command JRNY-015 names had no place at
 * all: delete was an icon button outside the menu, and it could only ever act
 * on the page's active version.
 */
describe('AgentPipelineVersionSelector — delete item', () => {
  it('renders no delete item when the caller does not offer one', async () => {
    const user = userEvent.setup();
    const { getByTestId, queryByTestId } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={2}
        versions={versions}
        onSelectVersion={vi.fn()}
      />,
    );
    await user.click(getByTestId('version-selector-trigger'));
    expect(queryByTestId('agent-version-delete')).not.toBeInTheDocument();
  });

  it('reports the SELECTED version to onDeleteVersion and closes the menu behind it', async () => {
    const user = userEvent.setup();
    const onDeleteVersion = vi.fn();
    const { getByTestId, queryByRole } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={2}
        versions={versions}
        onSelectVersion={vi.fn()}
        onDeleteVersion={onDeleteVersion}
      />,
    );
    await user.click(getByTestId('version-selector-trigger'));
    await user.click(getByTestId('agent-version-delete'));

    expect(onDeleteVersion).toHaveBeenCalledTimes(1);
    expect(onDeleteVersion.mock.calls[0]?.[0]).toMatchObject({ id: 2, name: 'v1' });
    // The caller answers with a confirm dialog; a menu left open behind it
    // stacks a second focus trap.
    expect(queryByRole('menu')).not.toBeInTheDocument();
  });

  it('follows the version the user picks, not the one the page opened on', async () => {
    const user = userEvent.setup();
    const onDeleteVersion = vi.fn();
    const { getByTestId, getByRole } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={2}
        versions={versions}
        onSelectVersion={vi.fn()}
        onDeleteVersion={onDeleteVersion}
      />,
    );

    // Re-open on a DIFFERENT selected version, the way the page re-renders
    // after a version switch. The old icon button always deleted the active
    // version; this item deletes whichever the menu marks.
    await user.click(getByTestId('version-selector-trigger'));
    expect(getByRole('menuitem', { name: /^v0/ })).toBeInTheDocument();
    await user.keyboard('{Escape}');

    await user.click(getByTestId('version-selector-trigger'));
    await user.click(getByTestId('agent-version-delete'));
    expect(onDeleteVersion.mock.calls[0]?.[0]).toMatchObject({ id: 2 });
  });

  it('refuses "base" and refuses the recorded default — lib/versionDeletion', async () => {
    const user = userEvent.setup();
    const base = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={1}
        versions={versions}
        onSelectVersion={vi.fn()}
        onDeleteVersion={vi.fn()}
      />,
    );
    await user.click(base.getByTestId('version-selector-trigger'));
    expect(base.getByTestId('agent-version-delete')).toHaveAttribute('aria-disabled', 'true');
    base.unmount();

    const isDefault = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={2}
        versions={versions}
        defaultVersionId={2}
        onSelectVersion={vi.fn()}
        onDeleteVersion={vi.fn()}
      />,
    );
    await user.click(isDefault.getByTestId('version-selector-trigger'));
    expect(isDefault.getByTestId('agent-version-delete')).toHaveAttribute('aria-disabled', 'true');
  });

  /*
   * The discriminating case for the rule above: a component that disabled the
   * item unconditionally would pass every refusal assertion.
   */
  it('allows an ordinary version while another one is the default', async () => {
    const user = userEvent.setup();
    const { getByTestId } = renderWithTheme(
      <AgentPipelineVersionSelector
        applicationVersionId={2}
        versions={versions}
        defaultVersionId={3}
        onSelectVersion={vi.fn()}
        onDeleteVersion={vi.fn()}
      />,
    );
    await user.click(getByTestId('version-selector-trigger'));
    expect(getByTestId('agent-version-delete')).not.toHaveAttribute('aria-disabled', 'true');
  });
});
