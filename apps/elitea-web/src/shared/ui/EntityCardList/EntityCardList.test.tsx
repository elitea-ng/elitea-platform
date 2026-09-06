import { fireEvent } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { EntityCard } from './EntityCard';
import { EntityCardList } from './EntityCardList';
import { EntityEmptyState } from './EntityEmptyState';
import { EntityListPagination } from './EntityListPagination';
import type { EntityListItem } from './model';

const ITEMS: readonly EntityListItem[] = [
  { id: '1', name: 'Meeting Prep Assistant', description: 'Prepares a call', createdAt: '2026-03-14T10:00:00Z' },
  { id: '2', name: 'Test Case Generator', description: 'Writes test cases', createdAt: '2026-03-15T10:00:00Z' },
];

describe('EntityCard', () => {
  it('renders the icon tile, the title, the author avatars and the tag chips', () => {
    const { getByTestId, getAllByTestId } = renderWithTheme(
      <EntityCard
        item={{
          id: '1',
          name: 'super-pipe',
          icon: <svg data-testid="glyph" />,
          authors: [{ id: 'a', name: 'Alexander Kharkevich' }],
          tags: [{ name: 'tag' }, { name: 'no1tag' }],
        }}
      />,
    );
    expect(getByTestId('entity-card-icon')).toBeInTheDocument();
    expect(getByTestId('entity-card-name')).toHaveTextContent('super-pipe');
    expect(getAllByTestId('entity-card-author-avatar')).toHaveLength(1);
    expect(getAllByTestId('entity-card-tag-chip').map((chip) => chip.textContent)).toEqual(['tag', 'no1tag']);
  });

  it('separates the author cluster from the tag strip with a divider, and drops it when there are no tags', () => {
    const withTags = renderWithTheme(
      <EntityCard item={{ id: '1', name: 'a', authors: [{ name: 'A' }], tags: [{ name: 't' }] }} />,
    );
    expect(withTags.getAllByTestId('entity-card-section-divider')).toHaveLength(1);
    withTags.unmount();

    const withoutTags = renderWithTheme(<EntityCard item={{ id: '2', name: 'b', authors: [{ name: 'A' }], tags: [] }} />);
    expect(withoutTags.queryAllByTestId('entity-card-section-divider')).toHaveLength(0);
  });

  it('caps the tag strip at two chips and counts the rest', () => {
    const { getAllByTestId, getByTestId } = renderWithTheme(
      <EntityCard item={{ id: '1', name: 'a', tags: [{ name: 'one' }, { name: 'two' }, { name: 'three' }] }} />,
    );
    expect(getAllByTestId('entity-card-tag-chip')).toHaveLength(2);
    expect(getByTestId('entity-card-tag-overflow')).toHaveTextContent('+1');
  });

  it('activates on click and on Enter', () => {
    const onClick = vi.fn();
    const { getByTestId } = renderWithTheme(<EntityCard item={{ id: '1', name: 'a', onClick }} />);
    fireEvent.click(getByTestId('entity-card'));
    fireEvent.keyDown(getByTestId('entity-card'), { key: 'Enter' });
    expect(onClick).toHaveBeenCalledTimes(2);
  });
});

describe('EntityCardList', () => {
  it('renders one card per item in the grid view', () => {
    const { getAllByTestId } = renderWithTheme(
      <EntityCardList
        items={ITEMS}
        view="cards"
      />,
    );
    expect(getAllByTestId('entity-card')).toHaveLength(2);
  });

  it('renders a real table (not a role-annotated div soup) in the table view', () => {
    const { getAllByTestId, container } = renderWithTheme(
      <EntityCardList
        items={ITEMS}
        view="table"
      />,
    );
    expect(container.querySelector('table')).toBeInTheDocument();
    expect(getAllByTestId('entity-list-row')).toHaveLength(2);
  });

  it('shows the empty state only when the list is genuinely empty — never while loading or erroring', () => {
    const empty = (
      <EntityEmptyState
        title="No agents yet"
        description="Create your first agent."
      />
    );
    const loading = renderWithTheme(
      <EntityCardList
        items={[]}
        isLoading
        emptyState={empty}
      />,
    );
    expect(loading.container.querySelector('[data-testid="entity-empty-state"]')).toBeNull();
    expect(loading.getAllByTestId('entity-card-skeleton').length).toBeGreaterThan(0);
    loading.unmount();

    const failed = renderWithTheme(
      <EntityCardList
        items={[]}
        isError
        errorMessage="Boom"
        emptyState={empty}
      />,
    );
    expect(failed.container.querySelector('[data-testid="entity-empty-state"]')).toBeNull();
    expect(failed.getByRole('alert')).toHaveTextContent('Boom');
    failed.unmount();

    const blank = renderWithTheme(
      <EntityCardList
        items={[]}
        emptyState={empty}
      />,
    );
    expect(blank.getByTestId('entity-empty-state')).toBeInTheDocument();
  });

  it('renders the pagination footer only once there are rows', () => {
    const pagination = { page: 0, pageSize: 20, total: 40, onPageChange: vi.fn(), onPageSizeChange: vi.fn() };
    const withRows = renderWithTheme(
      <EntityCardList
        items={ITEMS}
        pagination={pagination}
      />,
    );
    expect(withRows.getByTestId('entity-list-page-info')).toHaveTextContent('1 - 20 of 40');
    withRows.unmount();

    const withoutRows = renderWithTheme(
      <EntityCardList
        items={[]}
        pagination={pagination}
      />,
    );
    expect(withoutRows.container.querySelector('[data-testid="entity-list-pagination"]')).toBeNull();
  });
});

describe('EntityListPagination', () => {
  it('renders nothing at all when there is nothing to page through', () => {
    const { queryByTestId } = renderWithTheme(
      <EntityListPagination
        page={0}
        pageSize={20}
        total={0}
        onPageChange={vi.fn()}
        onPageSizeChange={vi.fn()}
      />,
    );
    expect(queryByTestId('entity-list-pagination')).not.toBeInTheDocument();
  });

  it('disables the arrow that would leave the range, and steps the page otherwise', () => {
    const onPageChange = vi.fn();
    const { getByRole } = renderWithTheme(
      <EntityListPagination
        page={0}
        pageSize={20}
        total={40}
        onPageChange={onPageChange}
        onPageSizeChange={vi.fn()}
      />,
    );
    expect(getByRole('button', { name: 'Previous page' })).toBeDisabled();
    fireEvent.click(getByRole('button', { name: 'Next page' }));
    expect(onPageChange).toHaveBeenCalledWith(1);
  });
});

describe('EntityEmptyState', () => {
  it('paints both illustration variants so the theme, not JavaScript, picks one', () => {
    const { container } = renderWithTheme(
      <EntityEmptyState
        art="skills"
        title="No skills yet"
        description="Create your first skill."
      />,
    );
    expect(container.querySelectorAll('img')).toHaveLength(2);
  });

  it('shows the Create CTA only when the caller can act on it', () => {
    const onCreateClick = vi.fn();
    const withCta = renderWithTheme(
      <EntityEmptyState
        title="No skills yet"
        description="…"
        onCreateClick={onCreateClick}
      />,
    );
    fireEvent.click(withCta.getByRole('button', { name: 'Create' }));
    expect(onCreateClick).toHaveBeenCalledTimes(1);
    withCta.unmount();

    const withoutCta = renderWithTheme(
      <EntityEmptyState
        title="No skills yet"
        description="…"
      />,
    );
    expect(withoutCta.container.querySelector('button')).toBeNull();
  });
});
