import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '../__tests__/testUtils';
import type { SkillRecord } from '../model/types';
import { SkillsList } from './SkillsList';

const skill: SkillRecord = {
  id: '1',
  project_id: 'p',
  name: 'Reviewer',
  description: 'Reviews code',
  type: 'skill',
  is_default: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
};

function renderList(overrides: Partial<Parameters<typeof SkillsList>[0]> = {}) {
  const props = {
    items: [skill],
    isLoading: false,
    isError: false,
    query: '',
    onSelect: vi.fn(),
    onDelete: vi.fn(),
    onExport: vi.fn(),
    ...overrides,
  };
  renderWithProviders(<SkillsList {...props} />);
  return props;
}

describe('SkillsList', () => {
  it('renders loading, failure, and both empty-state variants', () => {
    const { rerender, container } = renderWithProviders(
      <SkillsList
        items={[]}
        isLoading
        isError={false}
        query=""
        onSelect={vi.fn()}
        onDelete={vi.fn()}
        onExport={vi.fn()}
      />,
    );
    // The grid paints card skeletons while the first page loads, replacing the
    // "Loading skills…" text line the old <List> rendered.
    expect(container.querySelectorAll('[data-testid="entity-card-skeleton"]').length).toBeGreaterThan(0);
    rerender(
      <SkillsList
        items={[]}
        isLoading={false}
        isError
        query=""
        onSelect={vi.fn()}
        onDelete={vi.fn()}
        onExport={vi.fn()}
      />,
    );
    expect(screen.getByRole('alert')).toHaveTextContent('Failed');
    rerender(
      <SkillsList
        items={[]}
        isLoading={false}
        isError={false}
        query=""
        onSelect={vi.fn()}
        onDelete={vi.fn()}
        onExport={vi.fn()}
      />,
    );
    expect(screen.getByText('No skills yet')).toBeInTheDocument();
    rerender(
      <SkillsList
        items={[]}
        isLoading={false}
        isError={false}
        query="missing"
        onSelect={vi.fn()}
        onDelete={vi.fn()}
        onExport={vi.fn()}
      />,
    );
    expect(screen.getByText('No matching skills.')).toBeInTheDocument();
  });

  it('routes row, export, and delete actions to the caller', async () => {
    const user = userEvent.setup();
    const props = renderList();
    await user.click(screen.getByTestId('entity-card'));
    await user.click(screen.getByRole('button', { name: 'Export' }));
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    expect(props.onSelect).toHaveBeenCalledWith('1');
    expect(props.onExport).toHaveBeenCalledWith(skill);
    expect(props.onDelete).toHaveBeenCalledWith(skill);
  });

  it('uses an untitled fallback when the backend returns an empty name', () => {
    renderList({ items: [{ ...skill, name: '' }] });
    expect(screen.getByText('Untitled skill')).toBeInTheDocument();
  });

  it('offers Try on the card and routes it to the caller (skills epic 2169)', async () => {
    const onTry = vi.fn();
    renderList({ onTry });
    await userEvent.setup().click(screen.getByRole('button', { name: 'Try Reviewer' }));
    expect(onTry).toHaveBeenCalledWith(skill);
  });

  it('shows no Try without a caller to start it', () => {
    renderList();
    expect(screen.queryByRole('button', { name: 'Try Reviewer' })).not.toBeInTheDocument();
  });

  it('puts Try and Use in a chat in the row menu', async () => {
    window.history.pushState({}, '', '/?view=table');
    try {
      const user = userEvent.setup();
      const onTry = vi.fn();
      const onUseInChat = vi.fn();
      renderList({ onTry, onUseInChat });
      await user.click(screen.getByTestId('skill-row-menu'));
      await user.click(screen.getByRole('menuitem', { name: 'Try it' }));
      expect(onTry).toHaveBeenCalledWith(skill);
      await user.click(screen.getByTestId('skill-row-menu'));
      await user.click(screen.getByRole('menuitem', { name: 'Use in a chat' }));
      expect(onUseInChat).toHaveBeenCalledWith(skill);
    } finally {
      window.history.pushState({}, '', '/');
    }
  });

  // #2114: the table view carries an Actions column with a three-dot menu per row.
  it('offers a three-dot Actions menu per row in the table view (issue 2114)', async () => {
    window.history.pushState({}, '', '/?view=table');
    try {
      const user = userEvent.setup();
      const props = renderList();
      expect(screen.getByRole('columnheader', { name: 'Actions' })).toBeInTheDocument();
      await user.click(screen.getByTestId('skill-row-menu'));
      await user.click(screen.getByRole('menuitem', { name: 'Export' }));
      expect(props.onExport).toHaveBeenCalledWith(skill);
      await user.click(screen.getByTestId('skill-row-menu'));
      await user.click(screen.getByRole('menuitem', { name: 'Delete' }));
      expect(props.onDelete).toHaveBeenCalledWith(skill);
    } finally {
      window.history.pushState({}, '', '/');
    }
  });
});
