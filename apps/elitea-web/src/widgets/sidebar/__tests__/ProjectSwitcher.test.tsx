import userEvent from '@testing-library/user-event';
import { render, screen, waitFor, within } from '@testing-library/react';
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { Project } from '@/entities/project';
import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { ProjectSwitcher } from '../ui/ProjectSwitcher';

const projects: readonly Project[] = [
  { id: 11, name: 'Public', suspended: false },
  { id: 2, name: 'Acme', suspended: false },
];

/**
 * `renderWithTheme` is pinned to `DEFAULT_COLOR_SCHEME` ('dark'), so it cannot
 * express "the same component, light scheme". The light-scheme cases below
 * build their own provider rather than widen that shared helper, which every
 * other `shared/ui` test depends on.
 */
const schemeTheme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderInScheme(mode: 'light' | 'dark', ui: Parameters<typeof render>[0]): void {
  document.documentElement.setAttribute('data-el-scheme', mode);
  render(
    <ThemeProvider
      theme={schemeTheme}
      defaultMode={mode}
    >
      <CssBaseline />
      {ui}
    </ThemeProvider>,
  );
}

describe('ProjectSwitcher', () => {
  it('shows "No projects" when the list is empty', () => {
    renderWithTheme(
      <ProjectSwitcher
        projects={[]}
        selectedProjectId={undefined}
        onSelect={vi.fn()}
      />,
    );
    expect(screen.getByText('No projects')).toBeInTheDocument();
  });

  it('shows the selected project name', () => {
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );
    expect(screen.getByText('Acme')).toBeInTheDocument();
  });

  it('collapsed mode hides the text block', () => {
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
        collapsed
      />,
    );
    expect(screen.queryByText('Project:')).not.toBeInTheDocument();
  });

  /** R4 regression: `customRenderProject`'s `StyledTooltip` (`SidebarProjectSelect.jsx:25-63`). */
  it('R4: collapsed mode shows the selected project name in a hover tooltip', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
        collapsed
      />,
    );
    await user.hover(screen.getByRole('button'));
    const tooltip = await screen.findByRole('tooltip', undefined, { timeout: 2000 });
    expect(tooltip).toHaveTextContent('Acme');
  });

  it('R4: collapsed mode falls back to "No projects" in the tooltip when the list is empty', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <ProjectSwitcher
        projects={[]}
        selectedProjectId={undefined}
        onSelect={vi.fn()}
        collapsed
      />,
    );
    await user.hover(screen.getByRole('button'));
    const tooltip = await screen.findByRole('tooltip', undefined, { timeout: 2000 });
    expect(tooltip).toHaveTextContent('No projects');
  });

  it('R4: expanded mode shows no tooltip (title is already visible inline)', () => {
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();
  });

  /**
   * Issue #238 regression: the trigger must announce itself as a dropdown.
   * The pre-fix component carried neither attribute, so both halves of this
   * assertion failed against it.
   */
  it('issue 238: the trigger announces a listbox popup and reports the closed state', () => {
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );
    const trigger = screen.getByRole('button');
    expect(trigger).toHaveAttribute('aria-haspopup', 'listbox');
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    expect(trigger).not.toHaveAttribute('aria-controls');
  });

  it('issue 238: clicking the trigger flips aria-expanded to true and points aria-controls at the listbox', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );
    const trigger = screen.getByRole('button');
    await user.click(trigger);
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    const listbox = screen.getByRole('listbox');
    expect(trigger).toHaveAttribute('aria-controls', listbox.id);
    expect(listbox.id).not.toBe('');
  });

  it('issue 238: clicking the trigger a second time closes the popup and resets aria-expanded', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );
    const trigger = screen.getByRole('button');
    await user.click(trigger);
    await user.click(trigger);
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
  });

  /** Issue #238: without a visible arrow the control reads as static text. */
  it('issue 238: expanded mode draws the dropdown chevron', () => {
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );
    expect(screen.getByTestId('project-switcher-chevron')).toBeInTheDocument();
  });

  it('issue 238: the chevron rotates to indicate the open state', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );
    expect(screen.getByTestId('project-switcher-chevron')).toHaveStyle({ transform: 'rotate(0deg)' });
    await user.click(screen.getByRole('button'));
    expect(screen.getByTestId('project-switcher-chevron')).toHaveStyle({ transform: 'rotate(180deg)' });
  });

  it('issue 238: collapsed mode hides the chevron, leaving only the avatar', () => {
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
        collapsed
      />,
    );
    expect(screen.queryByTestId('project-switcher-chevron')).not.toBeInTheDocument();
  });

  it('opens the dropdown on click and lists every project', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );
    await user.click(screen.getByRole('button'));
    const listbox = screen.getByRole('listbox');
    // Each row is avatar-initial + name, so match on containment, not equality.
    const names = within(listbox)
      .getAllByRole('option')
      .map((option) => option.textContent ?? '');
    expect(names).toHaveLength(projects.length);
    expect(names[0]).toContain('Public');
    expect(names[1]).toContain('Acme');
    expect(screen.getByRole('option', { name: /Acme/ })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('option', { name: /Public/ })).toHaveAttribute('aria-selected', 'false');
  });

  /**
   * Issue #238: the popup used to be gated on `open && projects.length > 0`,
   * so an empty list turned the trigger into a silent no-op. It now opens on
   * a disabled "No projects" row, the same copy the trigger text falls back to.
   */
  it('issue 238: an empty project list opens a disabled "No projects" row, not nothing', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <ProjectSwitcher
        projects={[]}
        selectedProjectId={undefined}
        onSelect={vi.fn()}
      />,
    );
    await user.click(screen.getByRole('button'));
    expect(screen.getByRole('listbox')).toBeInTheDocument();
    const empty = screen.getByRole('option', { name: 'No projects' });
    expect(empty).toHaveAttribute('aria-disabled', 'true');
    expect(empty).toHaveAttribute('aria-selected', 'false');
  });

  it('issue 238: the empty "No projects" row does not select anything when clicked', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    renderWithTheme(
      <ProjectSwitcher
        projects={[]}
        selectedProjectId={undefined}
        onSelect={onSelect}
      />,
    );
    await user.click(screen.getByRole('button'));
    await user.click(screen.getByRole('option', { name: 'No projects' }));
    expect(onSelect).not.toHaveBeenCalled();
  });

  it('selecting an option calls onSelect and closes the dropdown', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={onSelect}
      />,
    );
    const trigger = screen.getByRole('button');
    await user.click(trigger);
    await user.click(screen.getByRole('option', { name: /Public/ }));
    expect(onSelect).toHaveBeenCalledWith('11', 'Public');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
  });

  it('selecting an option via the keyboard (Enter) also calls onSelect', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={onSelect}
      />,
    );
    await user.click(screen.getByRole('button'));
    screen.getByRole('option', { name: /Public/ }).focus();
    await user.keyboard('{Enter}');
    expect(onSelect).toHaveBeenCalledWith('11', 'Public');
  });

  /**
   * The chevron must not be a dark-scheme-only affordance. Nothing in the
   * component branches on the colour scheme, so these pin that fact rather
   * than describe a fix — see the scheme-parity guard below for the failure
   * mode that CAN make one scheme lose an icon.
   */
  it('issue 238: renders the chevron in the light colour scheme too', () => {
    renderInScheme(
      'light',
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );
    expect(screen.getByTestId('project-switcher-chevron')).toBeInTheDocument();
    expect(screen.getByRole('button')).toHaveAttribute('aria-haspopup', 'listbox');
  });

  it('issue 238: the light-scheme dropdown still opens and lists every project', async () => {
    const user = userEvent.setup();
    renderInScheme(
      'light',
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );
    await user.click(screen.getByRole('button'));
    expect(screen.getByRole('listbox')).toBeInTheDocument();
    expect(screen.getByRole('option', { name: /Public/ })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: /Acme/ })).toBeInTheDocument();
  });

  /**
   * The real way a sidebar icon can exist in one scheme and vanish in the
   * other: a palette token the brand pack declares for only ONE scheme. The
   * pack does exactly that today — `--el-palette-text-icon` is emitted under
   * `[data-el-scheme="dark"]` and NOT under `[data-el-scheme="light"]` — so a
   * chevron coloured from such a token would silently fall back to the baked
   * default-scheme value. This asserts every variable the chevron's own style
   * references is declared in BOTH scheme blocks.
   */
  it('issue 238: the chevron pins no colour of its own, so it cannot differ by scheme', () => {
    renderInScheme(
      'dark',
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );
    const chevronStyle = screen.getByTestId('project-switcher-chevron').getAttribute('style') ?? '';
    const inlineColour = /(^|;)\s*color\s*:/.test(chevronStyle);
    expect(inlineColour).toBe(false);

    // It still resolves through a variable — the one it INHERITS from the
    // trigger, which is the same colour the project name is painted in and is
    // therefore correct in both schemes by construction. What matters is that
    // every variable in that chain is declared under both scheme blocks, so
    // the glyph cannot fall back to a baked default-scheme value in one of
    // them.
    const declared = getComputedStyle(screen.getByTestId('project-switcher-chevron')).color;
    const used = [...declared.matchAll(/--[\w-]+/g)].map((match) => match[0]);
    expect(used.length).toBeGreaterThan(0);

    const sheets = schemeTheme.generateStyleSheets();
    const bySelector: Record<string, Record<string, string>> = {};
    for (const sheet of sheets) {
      for (const [selector, declarations] of Object.entries(sheet as Record<string, Record<string, string>>)) {
        bySelector[selector] = declarations;
      }
    }
    const darkBlock = Object.keys(bySelector[':root, [data-el-scheme="dark"]'] ?? {});
    const lightBlock = Object.keys(bySelector['[data-el-scheme="light"]'] ?? {});
    for (const variable of used) {
      expect(darkBlock).toContain(variable);
      expect(lightBlock).toContain(variable);
    }
  });

  /**
   * The trap the line above closes, kept measured rather than remembered.
   *
   * The brand pack does not declare every palette token under both schemes:
   * `--el-palette-text-icon` is emitted under `[data-el-scheme="dark"]` and
   * NOT under `[data-el-scheme="light"]`. Anything coloured from such a token
   * falls back to the baked default-scheme value, and in a screenshot a glyph
   * painted in the background colour is indistinguishable from a glyph that
   * never painted at all — which is exactly how the chevron read as missing in
   * the light visual baselines while every unit test showed it present.
   *
   * This asserts the asymmetry still exists, so the day the pack is fixed this
   * test fails and says so, instead of standing as a warning about a hazard
   * that has quietly gone away.
   */
  it('issue 238: the brand pack still declares at least one palette token in only one scheme', () => {
    const sheets = schemeTheme.generateStyleSheets();
    const bySelector: Record<string, Record<string, string>> = {};
    for (const sheet of sheets) {
      for (const [selector, declarations] of Object.entries(sheet as Record<string, Record<string, string>>)) {
        bySelector[selector] = declarations;
      }
    }
    const darkBlock = Object.keys(bySelector[':root, [data-el-scheme="dark"]'] ?? {});
    const lightBlock = new Set(Object.keys(bySelector['[data-el-scheme="light"]'] ?? {}));
    const darkOnly = darkBlock.filter((variable) => !lightBlock.has(variable));
    expect(darkOnly).toContain('--el-palette-text-icon');
  });

  it('clicking away closes the dropdown without selecting', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    renderWithTheme(
      <div>
        <ProjectSwitcher
          projects={projects}
          selectedProjectId="2"
          onSelect={onSelect}
        />
        <button type="button">outside</button>
      </div>,
    );
    await user.click(screen.getByRole('button', { name: /Acme/ }));
    expect(screen.getByRole('option', { name: /Public/ })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'outside' }));
    expect(screen.queryByRole('option')).not.toBeInTheDocument();
    expect(onSelect).not.toHaveBeenCalled();
  });

  /*
   * The panel used minWidth: '14rem' — 224px against a 208px rail — so it
   * always hung over the content beside the sidebar. It now follows the
   * trigger's measured width.
   */
  it('sizes the panel from the trigger rather than a fixed 14rem', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );

    await user.click(screen.getByRole('button', { name: /Acme/ }));

    const listbox = screen.getByRole('listbox');
    expect(listbox).toBeInTheDocument();
    // jsdom reports 0 for every measured box, so the assertion is on the
    // STYLE the component asked for, which is what regressed.
    expect(listbox.style.minWidth === '14rem').toBe(false);
  });

  /*
   * Selection was conveyed by aria-selected alone: a sighted user could not
   * tell which project was active. The icon matches SingleSelectMenuItem,
   * the shared dropdown row this control cannot reuse (no avatar support).
   */
  it('marks the selected project visibly, not only for assistive tech', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <ProjectSwitcher
        projects={projects}
        selectedProjectId="2"
        onSelect={vi.fn()}
      />,
    );

    await user.click(screen.getByRole('button', { name: /Acme/ }));

    // Two projects in the fixture, exactly one of them selected.
    const marks = screen.getAllByTestId('project-switcher-selected');
    expect(marks).toHaveLength(1);
    const selectedRow = screen.getByRole('option', { name: /Acme/ });
    expect(selectedRow).toContainElement(marks[0] ?? null);
  });
});

/**
 * "Request a project" (#871). A separate describe block because it needs a
 * `QueryClientProvider` (`RequestProjectDialog` only mounts, and only then
 * calls `useQuery`, once opened — see `ProjectSwitcher.tsx`'s own comment on
 * why every OTHER test above stays on the plain `renderWithTheme` that has
 * none).
 */
describe('ProjectSwitcher — request a project', () => {
  const BASE = '/api/v2';

  beforeEach(() => {
    configureGeneratedClient({ baseUrl: BASE });
  });
  afterEach(() => {
    resetGeneratedClient();
  });

  it('opens the request-a-project dialog from the footer row, closing the switcher popup', async () => {
    server.use(http.get(`${BASE}/admin/moderation_status/project_requests/mine`, () => HttpResponse.json({ total: 0, rows: [] })));
    const user = userEvent.setup();

    render(
      <AppProviders>
        <ProjectSwitcher
          projects={projects}
          selectedProjectId="2"
          onSelect={vi.fn()}
        />
      </AppProviders>,
    );

    await user.click(screen.getByRole('button', { name: /Acme/ }));
    await user.click(screen.getByTestId('project-switcher-request-project'));

    expect(await screen.findByTestId('request-project-dialog')).toBeInTheDocument();
    // The switcher's own popup closed — the two are mutually exclusive.
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();

    await waitFor(() => {
      expect(screen.getByText("You haven't requested a project yet.")).toBeInTheDocument();
    });
  });
});
