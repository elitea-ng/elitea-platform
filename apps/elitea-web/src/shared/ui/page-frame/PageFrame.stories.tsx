import Button from '@mui/material/Button';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { expect, within } from 'storybook/test';

import { PageFrame, PageHeader } from '.';

const meta = {
  title: 'shared/ui/PageFrame',
  component: PageHeader,
  parameters: { a11y: { test: 'error' } },
  args: { title: 'Projects' },
  decorators: [
    (Story) => (
      <PageFrame>
        <Story />
      </PageFrame>
    ),
  ],
} satisfies Meta<typeof PageHeader>;

export default meta;
type Story = StoryObj<typeof meta>;

export const TitleOnly: Story = {
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await expect(canvas.getByRole('heading', { level: 1, name: 'Projects' })).toBeInTheDocument();
  },
};

export const WithSubtitle: Story = {
  args: {
    title: 'E-mail',
    subtitle: 'The relay this deployment sends invitations and notices through.',
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await expect(canvas.getByText('The relay this deployment sends invitations and notices through.')).toBeInTheDocument();
  },
};

export const WithActions: Story = {
  args: {
    actions: <Button variant="contained">Create project</Button>,
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await expect(canvas.getByRole('button', { name: 'Create project' })).toBeInTheDocument();
  },
};
