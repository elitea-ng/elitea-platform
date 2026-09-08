import { fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { BasicAccordion, type AccordionItem } from '.';

const ITEMS: AccordionItem[] = [
  { title: 'First', content: 'First body' },
  { title: 'Second', content: 'Second body' },
];

describe('BasicAccordion', () => {
  it('renders one panel per item, each expanded by default', () => {
    const { getAllByRole, getByText } = renderWithTheme(<BasicAccordion items={ITEMS} />);
    expect(getAllByRole('button')).toHaveLength(2);
    expect(getByText('First body')).toBeVisible();
    expect(getByText('Second body')).toBeVisible();
  });

  it('renders nothing for an empty items array', () => {
    const { queryAllByRole } = renderWithTheme(<BasicAccordion items={[]} />);
    expect(queryAllByRole('button')).toHaveLength(0);
  });

  it('upper-cases the title text by default', () => {
    const { getByText } = renderWithTheme(<BasicAccordion items={[{ title: 'lowercase title', content: 'x' }]} />);
    expect(getByText('lowercase title')).toHaveStyle({ textTransform: 'uppercase' });
  });

  it('does not upper-case the title when uppercase=false', () => {
    const { getByText } = renderWithTheme(
      <BasicAccordion
        items={[{ title: 'lowercase title', content: 'x' }]}
        uppercase={false}
      />,
    );
    expect(getByText('lowercase title')).toHaveStyle({ textTransform: 'none' });
  });

  it('collapses each panel independently when defaultExpanded=false', async () => {
    const user = userEvent.setup();
    const { getByRole, getByText } = renderWithTheme(
      <BasicAccordion
        items={ITEMS}
        defaultExpanded={false}
      />,
    );
    expect(getByText('First body')).not.toBeVisible();
    await user.click(getByRole('button', { name: 'First' }));
    expect(getByText('First body')).toBeVisible();
  });

  it('labels each expanded region via the summary title (id/aria-controls wiring)', () => {
    const { getByRole } = renderWithTheme(<BasicAccordion items={[ITEMS[0] as AccordionItem]} />);
    expect(getByRole('region', { name: 'First' })).toBeInTheDocument();
  });

  it('renders a summaryAction node and shields it from toggling the panel', async () => {
    const user = userEvent.setup();
    const onActionClick = vi.fn();
    const { getByRole, getByText } = renderWithTheme(
      <BasicAccordion
        items={[
          {
            title: 'With action',
            content: 'Body',
            summaryAction: (
              // A plain native button: MUI's own `IconButton`/`BaseBtn` render
              // as `<button>` too (same nesting note above applies to any
              // real caller's choice here) but pulling in the full styled
              // button machinery is unnecessary for this test's purpose —
              // proving the propagation shield, not styling.
              <button
                type="button"
                onClick={onActionClick}
              >
                Action
              </button>
            ),
          },
        ]}
        defaultExpanded={false}
      />,
    );
    await user.click(getByRole('button', { name: 'Action' }));
    expect(onActionClick).toHaveBeenCalledTimes(1);
    // Clicking the action must not also toggle the accordion open.
    expect(getByText('Body')).not.toBeVisible();
  });

  it('shields the summaryAction region from mousedown-triggered toggling too', () => {
    const { getByText, getByRole } = renderWithTheme(
      <BasicAccordion
        items={[
          {
            title: 'With action',
            content: 'Body',
            summaryAction: (
              <button type="button">Action</button>
            ),
          },
        ]}
        defaultExpanded={false}
      />,
    );
    fireEvent.mouseDown(getByRole('button', { name: 'Action' }));
    expect(getByText('Body')).not.toBeVisible();
  });

  /*
   * THE STRUCTURAL HALF OF THE SHIELD, and the one axe measures.
   *
   * `nested-interactive` fails on any focusable descendant of the summary
   * button — a `<button>`, or a `role="button"` element with a `tabIndex`.
   * The propagation tests above pass either way, so this asserts the DOM
   * relationship itself: the action is a sibling of the summary, never a
   * child of it.
   */
  it('renders the summaryAction outside the summary button', () => {
    const { getByRole } = renderWithTheme(
      <BasicAccordion
        items={[
          {
            title: 'With action',
            content: 'Body',
            summaryAction: <button type="button">Action</button>,
          },
        ]}
      />,
    );
    const summary = getByRole('button', { name: 'With action' });
    expect(summary).not.toContainElement(getByRole('button', { name: 'Action' }));
    expect(summary.querySelector('button, [tabindex], a[href]')).toBeNull();
  });

  it('forwards a controlled expanded/onChange pair to every panel', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { getByRole } = renderWithTheme(
      <BasicAccordion
        items={ITEMS}
        expanded={false}
        onChange={onChange}
      />,
    );
    await user.click(getByRole('button', { name: 'First' }));
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange.mock.calls[0]?.[1]).toBe(true);
  });

  it('applies slotSx.root and forwards data-testid', () => {
    const { getByTestId } = renderWithTheme(
      <BasicAccordion
        items={ITEMS}
        data-testid="accordion-list"
        slotSx={{ root: { marginTop: '1rem' } }}
      />,
    );
    expect(getByTestId('accordion-list')).toBeInTheDocument();
  });

  it('accepts showMode="right"', () => {
    const { getByRole } = renderWithTheme(
      <BasicAccordion
        items={[ITEMS[0] as AccordionItem]}
        showMode="right"
      />,
    );
    expect(getByRole('button', { name: 'First' })).toBeInTheDocument();
  });
});
