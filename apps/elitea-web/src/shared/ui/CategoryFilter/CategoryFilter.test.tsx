import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { CategoryFilter } from '.';

describe('CategoryFilter', () => {
  it('renders the title when given', () => {
    const { getByText } = renderWithTheme(<CategoryFilter title="Prompt library" />);
    expect(getByText('Prompt library')).toBeInTheDocument();
  });

  it('renders no title element when omitted', () => {
    const { queryByText } = renderWithTheme(<CategoryFilter />);
    expect(queryByText('Prompt library')).not.toBeInTheDocument();
  });

  it('gives the search field an accessible name from the placeholder', () => {
    const { getByRole } = renderWithTheme(
      <CategoryFilter
        searchPlaceholder="Search prompts"
        onSearchChange={vi.fn()}
      />,
    );
    expect(getByRole('textbox', { name: 'Search prompts' })).toBeInTheDocument();
  });

  it('draws no search box for a caller that does not handle one', () => {
    const { queryByRole } = renderWithTheme(<CategoryFilter searchPlaceholder="Search prompts" />);
    expect(queryByRole('textbox')).toBeNull();
  });

  it('calls onSearchChange as the user types', async () => {
    const user = userEvent.setup();
    const onSearchChange = vi.fn();
    const { getByRole } = renderWithTheme(
      <CategoryFilter
        searchPlaceholder="Search"
        onSearchChange={onSearchChange}
      />,
    );
    await user.type(getByRole('textbox', { name: 'Search' }), 'a');
    expect(onSearchChange).toHaveBeenCalled();
  });

  it('hides the category chip row when there is one or zero categories', () => {
    const { queryByText, rerender } = renderWithTheme(<CategoryFilter allCategories={['Writing']} />);
    expect(queryByText('Writing')).not.toBeInTheDocument();
    rerender(<CategoryFilter allCategories={[]} />);
    expect(queryByText('Writing')).not.toBeInTheDocument();
  });

  it('shows the category chip row when there is more than one category', () => {
    const { getByText } = renderWithTheme(<CategoryFilter allCategories={['Writing', 'Coding']} />);
    expect(getByText('Writing')).toBeInTheDocument();
    expect(getByText('Coding')).toBeInTheDocument();
  });

  it('marks the selected category chip aria-pressed=true and others false', () => {
    const { getByText } = renderWithTheme(
      <CategoryFilter
        allCategories={['Writing', 'Coding']}
        selectedCategories={['Coding']}
      />,
    );
    expect(getByText('Writing').closest('[aria-pressed]')).toHaveAttribute('aria-pressed', 'false');
    expect(getByText('Coding').closest('[aria-pressed]')).toHaveAttribute('aria-pressed', 'true');
  });

  it('calls onSelectCategory with the clicked category', async () => {
    const user = userEvent.setup();
    const onSelectCategory = vi.fn();
    const { getByText } = renderWithTheme(
      <CategoryFilter
        allCategories={['Writing', 'Coding']}
        onSelectCategory={onSelectCategory}
      />,
    );
    await user.click(getByText('Writing'));
    expect(onSelectCategory).toHaveBeenCalledWith('Writing');
  });

  it('renders children inside the scrollable items container', () => {
    const { getByTestId } = renderWithTheme(
      <CategoryFilter>
        <div data-testid="prompt-card">Card</div>
      </CategoryFilter>,
    );
    expect(getByTestId('prompt-card')).toBeInTheDocument();
  });
});
