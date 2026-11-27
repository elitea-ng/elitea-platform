import type { Meta, StoryObj } from '@storybook/react-vite';
import Box from '@mui/material/Box';
import { expect, within } from 'storybook/test';

import { AttentionIcon } from '@/shared/ui/icons/attention-icon';
import { RemoveIcon } from '@/shared/ui/icons/remove-icon';
import { SuccessIcon } from '@/shared/ui/icons/success-icon';

import { StatusChip, StatusDot, type StatusChipVariant, type StatusTone } from '.';

const STATES: readonly { tone: StatusTone; label: string; icon?: 'success' | 'attention' | 'remove' }[] = [
  { tone: 'success', label: 'Active', icon: 'success' },
  { tone: 'warning', label: 'Expires soon', icon: 'attention' },
  { tone: 'danger', label: 'Failed', icon: 'remove' },
  { tone: 'info', label: 'Queued' },
  { tone: 'neutral', label: 'Expired', icon: 'remove' },
];

function iconOf(name: (typeof STATES)[number]['icon']) {
  if (name === 'success') return <SuccessIcon />;
  if (name === 'attention') return <AttentionIcon />;
  if (name === 'remove') return <RemoveIcon />;
  return undefined;
}

interface DemoProps {
  readonly variant: StatusChipVariant;
  readonly withIcons: boolean;
}

/** Every tone in one variant, with the legacy icons or with dots. */
function Demo({ variant, withIcons }: DemoProps) {
  return (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 2 }}>
      {STATES.map((state) => (
        <StatusChip
          key={state.tone}
          tone={state.tone}
          label={state.label}
          icon={withIcons ? iconOf(state.icon) : undefined}
          variant={variant}
          muted={variant === 'label' && state.tone === 'neutral'}
          data-testid={`status-${state.tone}`}
        />
      ))}
    </Box>
  );
}

const meta = {
  title: 'shared/ui/StatusChip',
  component: Demo,
  parameters: { a11y: { test: 'error' } },
  args: { variant: 'label', withIcons: true },
} satisfies Meta<typeof Demo>;

export default meta;
type Story = StoryObj<typeof meta>;

/** The default: icon or dot in the tone's colour, the word in body text (legacy token expiry, run history). */
export const Label: Story = {
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await expect(canvas.getByText('Active')).toBeVisible();
    await expect(canvas.getByTestId('status-neutral')).toHaveAttribute('data-tone', 'neutral');
  },
};

export const LabelWithDots: Story = { args: { withIcons: false } };

/** The legacy schedule run chip. */
export const Chip: Story = { args: { variant: 'chip' } };

export const Text: Story = { args: { variant: 'text' } };

/** The icon alone; the word stays for screen readers. */
export const IconOnly: Story = {
  args: { variant: 'icon' },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await expect(canvas.getByText('Failed')).toBeInTheDocument();
  },
};

export const Dot: Story = {
  render: () => (
    <Box sx={{ display: 'flex', gap: 2 }}>
      {STATES.map((state) => (
        <StatusDot
          key={state.tone}
          tone={state.tone}
        />
      ))}
    </Box>
  ),
};
