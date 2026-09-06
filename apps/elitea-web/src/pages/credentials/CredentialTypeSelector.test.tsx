import { fireEvent, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CredentialTypeSelector } from './CredentialTypeSelector';
import type { ConfigurationTypeDescriptor } from '@/features/credentials';

class ResizeObserverStub {
  observe(): void {
    // no-op
  }
  disconnect(): void {
    // no-op
  }
}

beforeEach(() => {
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

const TYPES: ConfigurationTypeDescriptor[] = [
  { type: 'openai', config_schema: { title: 'OpenAI', properties: { data: { metadata: { categories: ['LLM'] } } } } },
  { type: 'azure', config_schema: { title: 'Azure', properties: { data: { metadata: { categories: ['LLM'] } } } } },
  { type: 'hidden_one', config_schema: { title: 'Hidden', metadata: { hidden: true }, properties: {} } },
];

describe('CredentialTypeSelector', () => {
  it('lists every visible type, sorted, and calls onSelectType when one is clicked', () => {
    const onSelectType = vi.fn();
    renderWithTheme(
      <CredentialTypeSelector
        configurationsData={TYPES}
        isFetching={false}
        onSelectType={onSelectType}
      />,
    );
    expect(screen.getByText('Azure')).toBeInTheDocument();
    expect(screen.getByText('OpenAI')).toBeInTheDocument();
    fireEvent.click(screen.getByText('OpenAI'));
    expect(onSelectType).toHaveBeenCalledWith('openai');
  });

  /**
   * [visual-parity regression] The baseline renders this picker through
   * `Category.GroupedCategory`: a centred "Choose the credentials type"
   * heading, a category-chip row, uppercase per-category section headings and
   * a leading brand glyph on every tile. This port rendered a bare `<div>`
   * with a `SimpleSearchBar` and an ungrouped list, so none of the four
   * existed. Every assertion below fails against the pre-fix component.
   */
  it('renders the catalogue chrome: title, a chip per category, per-category headings and a tile icon', () => {
    const twoCategories: ConfigurationTypeDescriptor[] = [
      { type: 'github', config_schema: { title: 'GitHub', properties: { data: { metadata: { categories: ['Code Repositories'] } } } } },
      { type: 'slack', config_schema: { title: 'Slack', properties: { data: { metadata: { categories: ['Communication'] } } } } },
    ];
    renderWithTheme(
      <CredentialTypeSelector
        configurationsData={twoCategories}
        isFetching={false}
        onSelectType={vi.fn()}
      />,
    );

    expect(screen.getByText('Choose the credentials type')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Code Repositories' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Communication' })).toBeInTheDocument();
    // One chip + one section heading per category.
    expect(screen.getAllByText('Code Repositories')).toHaveLength(2);
    expect(screen.getAllByText('Communication')).toHaveLength(2);
    expect(screen.getByText('GitHub').closest('button')?.querySelector('svg')).not.toBeNull();
  });

  it('hides types marked config_schema.metadata.hidden', () => {
    renderWithTheme(
      <CredentialTypeSelector
        configurationsData={TYPES}
        isFetching={false}
        onSelectType={vi.fn()}
      />,
    );
    expect(screen.queryByText('Hidden')).not.toBeInTheDocument();
  });

  it('filters by the search box', async () => {
    renderWithTheme(
      <CredentialTypeSelector
        configurationsData={TYPES}
        isFetching={false}
        onSelectType={vi.fn()}
      />,
    );
    fireEvent.change(screen.getByPlaceholderText('Search credentials'), { target: { value: 'azure' } });
    await waitFor(() => expect(screen.queryByText('OpenAI')).not.toBeInTheDocument());
    expect(screen.getByText('Azure')).toBeInTheDocument();
  });

  it('shows the no-results message when nothing matches', async () => {
    renderWithTheme(
      <CredentialTypeSelector
        configurationsData={TYPES}
        isFetching={false}
        onSelectType={vi.fn()}
      />,
    );
    fireEvent.change(screen.getByPlaceholderText('Search credentials'), { target: { value: 'zzz-no-match' } });
    await waitFor(() => expect(screen.getByText('No credentials found')).toBeInTheDocument());
  });

  /**
   * #131: the mounted /configurations/available/ route returned rows with no
   * `config_schema` at all, and the unguarded `item.config_schema.metadata`
   * read threw "Cannot read properties of undefined (reading 'metadata')" —
   * taking out the whole /settings/create-configuration route, which has no
   * error boundary below it. The cast is deliberate: `config_schema` is
   * REQUIRED by `ConfigurationTypeDescriptor`, so this is exactly the
   * type-system-versus-wire mismatch the guard exists for.
   */
  it('renders a schema-less catalogue entry by type instead of crashing', () => {
    const onSelectType = vi.fn();
    const malformed = [
      { type: 'openai', display_name: 'OpenAI', section: 'llm' },
      { type: 'chroma', display_name: 'Chroma', section: 'vectorstorage' },
    ] as unknown as ConfigurationTypeDescriptor[];

    renderWithTheme(
      <CredentialTypeSelector
        configurationsData={malformed}
        isFetching={false}
        onSelectType={onSelectType}
      />,
    );

    expect(screen.getByText('openai')).toBeInTheDocument();
    expect(screen.getByText('chroma')).toBeInTheDocument();
    fireEvent.click(screen.getByText('openai'));
    expect(onSelectType).toHaveBeenCalledWith('openai');
  });

  it('keeps rendering well-formed entries when one entry has no config_schema', () => {
    const mixed = [
      ...TYPES,
      { type: 'broken_one' },
    ] as unknown as ConfigurationTypeDescriptor[];

    renderWithTheme(
      <CredentialTypeSelector
        configurationsData={mixed}
        isFetching={false}
        onSelectType={vi.fn()}
      />,
    );

    expect(screen.getByText('OpenAI')).toBeInTheDocument();
    expect(screen.getByText('Azure')).toBeInTheDocument();
    expect(screen.getByText('broken_one')).toBeInTheDocument();
  });

  it('handles an undefined configurationsData without crashing', () => {
    renderWithTheme(
      <CredentialTypeSelector
        configurationsData={undefined}
        isFetching
        onSelectType={vi.fn()}
      />,
    );
    expect(screen.getByPlaceholderText('Search credentials')).toBeInTheDocument();
  });
});
