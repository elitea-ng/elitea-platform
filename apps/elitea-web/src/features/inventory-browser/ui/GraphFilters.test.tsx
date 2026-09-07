/**
 * The four controls that narrow the entity list.
 *
 * The search box SUBMITS; it does not filter as you type. Every keystroke here
 * is an INVOCATION on the provider — an engine call, not a cached GET — so a
 * typed word run live is eight runs of the graph matcher. If the draft ever
 * moves into the shared filter, the tests below are what notices: they type
 * without submitting and assert that nothing was raised.
 *
 * Each facet also keeps an "Any …" choice, which is what CLEARS it. Without an
 * empty option a user who picks a type can never get back to the whole graph.
 */
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '../__tests__/testUtils';
import { EMPTY_GRAPH_FILTER } from '../model/useGraphBrowser';
import { GraphFilters } from './GraphFilters';

function show(overrides: Partial<React.ComponentProps<typeof GraphFilters>> = {}) {
  const onChange = vi.fn();
  renderWithProviders(
    <GraphFilters
      filter={EMPTY_GRAPH_FILTER}
      types={['class', 'function']}
      layers={['application']}
      sources={['code', 'docs']}
      onChange={onChange}
      {...overrides}
    />,
  );
  return onChange;
}

/**
 * The search box itself. The test id sits on MUI's FormControl root — a
 * TextField forwards an unknown prop there, not to the `<input>` — so the field
 * has to be reached through it.
 */
function searchInput(): HTMLElement {
  // The test id is on the INPUT itself, not on the MUI TextField wrapper: an
  // unrecognised prop passed to TextField lands on the root FormControl div,
  // which cannot be typed into and is never disabled.
  return screen.getByTestId('inventory-search-input');
}

/** Open one facet's menu and click an option by its label. */
async function chooseFacet(
  user: ReturnType<typeof userEvent.setup>,
  testId: string,
  option: string,
): Promise<void> {
  const input = screen.getByTestId(testId);
  const combobox = input.parentElement?.querySelector('[role="combobox"]');
  await user.click(combobox as HTMLElement);
  const listbox = await screen.findByRole('listbox');
  await user.click(within(listbox).getByText(option));
}

describe('GraphFilters', () => {
  it('does NOT raise a filter while the query is being typed', async () => {
    // A live query is one invocation per keystroke.
    const user = userEvent.setup();
    const onChange = show();
    await user.type(searchInput(), 'checkout');
    expect(onChange).not.toHaveBeenCalled();
  });

  it('raises the trimmed query when the search is submitted', async () => {
    const user = userEvent.setup();
    const onChange = show();
    await user.type(searchInput(), '  checkout  ');
    await user.click(screen.getByTestId('inventory-search-submit'));
    expect(onChange).toHaveBeenCalledWith({ ...EMPTY_GRAPH_FILTER, query: 'checkout' });
  });

  it('raises a chosen type without disturbing the rest of the filter', async () => {
    const user = userEvent.setup();
    const filter = { ...EMPTY_GRAPH_FILTER, query: 'checkout', sourceToolkit: 'code' };
    const onChange = show({ filter });
    await chooseFacet(user, 'inventory-filter-type', 'class');
    expect(onChange).toHaveBeenCalledWith({ ...filter, entityType: 'class' });
  });

  it('raises a chosen layer and a chosen source', async () => {
    const user = userEvent.setup();
    const onChange = show();
    await chooseFacet(user, 'inventory-filter-layer', 'application');
    expect(onChange).toHaveBeenLastCalledWith({ ...EMPTY_GRAPH_FILTER, layer: 'application' });
    await chooseFacet(user, 'inventory-filter-source', 'docs');
    expect(onChange).toHaveBeenLastCalledWith({ ...EMPTY_GRAPH_FILTER, sourceToolkit: 'docs' });
  });

  it('offers an "Any …" choice, which is the only way to clear a facet', async () => {
    const user = userEvent.setup();
    const onChange = show({ filter: { ...EMPTY_GRAPH_FILTER, entityType: 'class' } });
    await chooseFacet(user, 'inventory-filter-type', 'Any type');
    expect(onChange).toHaveBeenCalledWith({ ...EMPTY_GRAPH_FILTER, entityType: '' });
  });

  it('renders a facet with no options rather than hiding it', async () => {
    // An empty select says "this graph holds no layers"; a control that
    // disappears says nothing and leaves the user hunting for it.
    const user = userEvent.setup();
    show({ layers: [] });
    expect(screen.getByTestId('inventory-filter-layer')).toBeInTheDocument();
    await chooseFacet(user, 'inventory-filter-layer', 'Any layer');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
  });

  it('opens with the query already in the box', () => {
    show({ filter: { ...EMPTY_GRAPH_FILTER, query: 'checkout' } });
    expect(searchInput()).toHaveValue('checkout');
  });
});
