import { useState } from 'react';

import type { Meta, StoryObj } from '@storybook/react-vite';
import { expect, fn, userEvent, within } from 'storybook/test';

import { ShowMoreButton } from '.';

const meta = {
  title: 'shared/ui/buttons/ShowMoreButton',
  component: ShowMoreButton,
  parameters: { a11y: { test: 'error' } },
  args: { expanded: false, onClick: fn() },
} satisfies Meta<typeof ShowMoreButton>;

export default meta;
type Story = StoryObj<typeof meta>;

export const Collapsed: Story = {};

export const Expanded: Story = {
  args: { expanded: true },
};

export const CustomLabels: Story = {
  args: { moreLabel: 'Show all', lessLabel: 'Show fewer' },
};

/** Toggles between "Show more" and "Show less" on click. */
export const Toggle: Story = {
  render: (args) => {
    function ToggleButton() {
      const [expanded, setExpanded] = useState(false);
      return (
        <ShowMoreButton
          {...args}
          expanded={expanded}
          onClick={(event) => {
            setExpanded((value) => !value);
            args.onClick(event);
          }}
        />
      );
    }
    return <ToggleButton />;
  },
  play: async ({ canvasElement, args }) => {
    const canvas = within(canvasElement);
    await userEvent.click(canvas.getByRole('button', { name: 'Show more' }));
    await expect(canvas.getByRole('button', { name: 'Show less' })).toHaveAttribute('aria-expanded', 'true');
    await expect(args.onClick).toHaveBeenCalledTimes(1);
  },
};
