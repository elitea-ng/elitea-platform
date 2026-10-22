import { useState } from 'react';

import type { Meta, StoryObj } from '@storybook/react-vite';
import { expect, fn, userEvent, within } from 'storybook/test';

import { SecretField } from '.';

const secretOptions = [
  { label: 'Prod API key', value: '{{secret.prod_api_key}}' },
  { label: 'Staging API key', value: '{{secret.staging_api_key}}' },
];

const meta = {
  title: 'shared/ui/secret-field/SecretField',
  component: SecretField,
  parameters: { a11y: { test: 'error' } },
  args: {
    value: '',
    label: 'API key',
    onChange: fn(),
  },
} satisfies Meta<typeof SecretField>;

export default meta;
type Story = StoryObj<typeof meta>;

export const PlainMaskedField: Story = {
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await expect(canvas.getByLabelText('API key', { exact: false })).toHaveAttribute('type', 'password');
  },
};

export const RevealToggle: Story = {
  args: { value: 's3cr3t-value' },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    const input = canvas.getByLabelText('API key', { exact: false });
    await expect(input).toHaveAttribute('type', 'password');
    await userEvent.click(canvas.getByRole('button', { name: 'Show value' }));
    await expect(input).toHaveAttribute('type', 'text');
  },
};

export const WithSecretPicker: Story = {
  args: {
    value: '{{secret.prod_api_key}}',
    secrets: { options: secretOptions, onRefresh: fn() },
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await expect(canvas.getByRole('combobox')).toBeInTheDocument();
    await expect(canvas.getByRole('button', { name: 'Refresh secrets' })).toBeInTheDocument();
  },
};

export const WithCreateSecretOption: Story = {
  args: {
    value: '{{secret.prod_api_key}}',
    secrets: {
      options: secretOptions,
      canCreate: true,
      onCreate: fn(),
      createLabel: 'Create new secret',
    },
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await userEvent.click(canvas.getByRole('combobox'));
    const listbox = within(canvasElement.ownerDocument.body);
    await expect(listbox.getByRole('option', { name: 'Create new secret' })).toBeInTheDocument();
  },
};

export const ErrorState: Story = {
  args: { error: true, helperText: 'This field is required' },
  // [T1 follow-up] Was waived here as `a11y: { test: 'off' }`: the token
  // `theme.vars.palette.icon.fill.error` this state reads (via
  // `MuiTextField`/`MuiFormHelperText`'s `Mui-error` styling) was `#D71616`
  // in BOTH colour schemes, 3.55:1 against the dark scheme's surface —
  // short of WCAG AA's 4.5:1 for this text size. Fixed at the token: the
  // dark scheme's `icon.fill.error` is now `#ED4F4F` (same hue/saturation,
  // raised lightness — the contrast overrides in
  // `src/shared/brand/tokens/README.md`, `parity/brand-hue-map.md` §10), 5.05:1 against
  // `background.default`. This story now runs the real a11y gate.
};

/** A realistic controlled usage: state lives with the caller, exactly as the props/callbacks contract expects. */
export const Controlled: Story = {
  render: (args) => {
    function ControlledSecretField() {
      const [value, setValue] = useState('');
      return (
        <SecretField
          {...args}
          value={value}
          onChange={setValue}
        />
      );
    }
    return <ControlledSecretField />;
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    const input = canvas.getByLabelText('API key', { exact: false });
    await userEvent.type(input, 'sk-live-123');
    await expect(input).toHaveValue('sk-live-123');
  },
};
