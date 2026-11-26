import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { PageFrame, PageHeader } from '.';

describe('PageFrame', () => {
  it('pads the page on the spacing scale', () => {
    const { getByRole } = renderWithTheme(
      <PageFrame>
        <PageHeader title="E-mail" />
      </PageFrame>,
    );
    const frame = getComputedStyle(getByRole('heading', { level: 1 }).parentElement as HTMLElement);
    // jsdom keeps the theme's calc(): the steps 2 and 3 of the scale (16 and
    // 24 px in comfortable density, 12 and 18 in compact). jsdom drops `gap`;
    // it travels in the same sx object as the padding.
    const step = (n: number) => `calc(${n} * var(--el-spacing))`;
    expect([frame.paddingTop, frame.paddingRight, frame.paddingBottom, frame.paddingLeft]).toEqual([
      step(2),
      step(3),
      step(2),
      step(3),
    ]);
  });
});

describe('PageHeader', () => {
  it('renders the title as the one h1, and the subtitle', () => {
    const { getAllByRole, getByText } = renderWithTheme(
      <PageFrame>
        <PageHeader title="E-mail" subtitle="The relay" />
      </PageFrame>,
    );
    const headings = getAllByRole('heading', { level: 1 });
    expect(headings).toHaveLength(1);
    expect(headings[0]).toHaveTextContent('E-mail');
    expect(getByText('The relay')).toBeInTheDocument();
  });

  it('puts the actions in the title row', () => {
    const { getByRole } = renderWithTheme(
      <PageHeader title="Projects" actions={<button type="button">Create</button>} />,
    );
    const heading = getByRole('heading', { level: 1, name: 'Projects' });
    expect(heading.parentElement).toContainElement(getByRole('button', { name: 'Create' }));
  });

  it('renders no subtitle and no row when they are not given', () => {
    const { getByRole, container } = renderWithTheme(<PageHeader title="Features" />);
    expect(container.childElementCount).toBe(1);
    expect(getByRole('heading', { level: 1 }).parentElement).toBe(container);
  });
});
