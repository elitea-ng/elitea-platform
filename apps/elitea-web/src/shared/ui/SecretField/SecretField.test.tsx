import userEvent from '@testing-library/user-event';
import { fireEvent } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { SecretField } from '.';

describe('SecretField', () => {
  describe('without a secrets config (plain masked field)', () => {
    it('renders a password input with no mode toggle', () => {
      const { getByLabelText, queryByRole } = renderWithTheme(
        <SecretField
          value=""
          onChange={() => {}}
          label="API key"
        />,
      );
      expect(getByLabelText('API key', { exact: false })).toHaveAttribute('type', 'password');
      expect(queryByRole('group')).not.toBeInTheDocument();
    });

    it('fires onChange as the user types, stripped to printable ASCII', () => {
      const onChange = vi.fn();
      const { getByLabelText } = renderWithTheme(
        <SecretField
          value=""
          onChange={onChange}
          label="API key"
        />,
      );
      const input = getByLabelText('API key', { exact: false });
      fireEvent.change(input, { target: { value: 'sk-\x01\x07live' } });
      expect(onChange).toHaveBeenCalledWith('sk-live');
    });

    it('fires onSave on blur', async () => {
      const user = userEvent.setup();
      const onSave = vi.fn();
      const { getByLabelText } = renderWithTheme(
        <SecretField
          value=""
          onChange={() => {}}
          label="API key"
          onSave={onSave}
        />,
      );
      const input = getByLabelText('API key', { exact: false });
      await user.click(input);
      await user.tab();
      expect(onSave).toHaveBeenCalledTimes(1);
    });

    it('renders the given helperText and error state', () => {
      const { getByText, getByLabelText } = renderWithTheme(
        <SecretField
          value=""
          onChange={() => {}}
          label="API key"
          error
          helperText="Required"
        />,
      );
      expect(getByText('Required')).toBeInTheDocument();
      expect(getByLabelText('API key', { exact: false })).toHaveAttribute('aria-invalid', 'true');
    });
  });

  describe('reveal/mask toggle', () => {
    it('starts masked, toggles to plain text on click, and back to masked — firing onReveal only when it becomes visible', async () => {
      const user = userEvent.setup();
      const onReveal = vi.fn();
      const { getByLabelText, getByRole } = renderWithTheme(
        <SecretField
          value="s3cr3t"
          onChange={() => {}}
          label="API key"
          onReveal={onReveal}
        />,
      );
      const input = getByLabelText('API key', { exact: false });
      expect(input).toHaveAttribute('type', 'password');

      const toggle = getByRole('button', { name: 'Show value' });
      await user.click(toggle);
      expect(input).toHaveAttribute('type', 'text');
      expect(onReveal).toHaveBeenCalledTimes(1);

      await user.click(getByRole('button', { name: 'Hide value' }));
      expect(input).toHaveAttribute('type', 'password');
      expect(onReveal).toHaveBeenCalledTimes(1);
    });

    it('hides the toggle button when passwordVisibilityToggle is false', () => {
      const { queryByRole } = renderWithTheme(
        <SecretField
          value="s3cr3t"
          onChange={() => {}}
          label="API key"
          passwordVisibilityToggle={false}
        />,
      );
      expect(queryByRole('button', { name: 'Show value' })).not.toBeInTheDocument();
    });
  });

  describe('with a secrets config', () => {
    const secretOptions = [
      { label: 'Prod API key', value: '{{secret.prod_api_key}}' },
      { label: 'Staging API key', value: '{{secret.staging_api_key}}' },
    ];

    it('renders the mode toggle and starts in password mode for a non-reference value', () => {
      const { getByRole, getByLabelText } = renderWithTheme(
        <SecretField
          value=""
          onChange={() => {}}
          label="API key"
          secrets={{ options: secretOptions }}
        />,
      );
      expect(getByRole('group', { name: 'Value type' })).toBeInTheDocument();
      expect(getByLabelText('API key', { exact: false })).toHaveAttribute('type', 'password');
    });

    it('starts in secret mode when the value already looks like a secret reference', () => {
      const { getByRole } = renderWithTheme(
        <SecretField
          value="{{secret.prod_api_key}}"
          onChange={() => {}}
          label="API key"
          secrets={{ options: secretOptions }}
        />,
      );
      expect(getByRole('combobox')).toBeInTheDocument();
    });

    it('switches modes on toggle click and clears the value', async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      const { getByRole } = renderWithTheme(
        <SecretField
          value=""
          onChange={onChange}
          label="API key"
          secrets={{ options: secretOptions }}
        />,
      );
      await user.click(getByRole('button', { name: 'Secret' }));
      expect(onChange).toHaveBeenCalledWith('');
      expect(getByRole('combobox')).toBeInTheDocument();
    });

    it('renders an empty option list without crashing when secrets.options is omitted', async () => {
      const user = userEvent.setup();
      const { getByRole, findByRole } = renderWithTheme(
        <SecretField
          value="{{secret.prod_api_key}}"
          onChange={() => {}}
          label="API key"
          secrets={{}}
        />,
      );
      await user.click(getByRole('combobox'));
      const listbox = await findByRole('listbox');
      expect(listbox.children).toHaveLength(0);
    });

    it('does nothing when the already-active tab is clicked again', async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      const { getByRole } = renderWithTheme(
        <SecretField
          value=""
          onChange={onChange}
          label="API key"
          secrets={{ options: secretOptions }}
        />,
      );
      // Starts in password mode already — clicking "Password" again is a no-op.
      await user.click(getByRole('button', { name: 'Password' }));
      expect(onChange).not.toHaveBeenCalled();
    });

    it('uses the given name for the select id, so its label stays associated', () => {
      const { getByRole } = renderWithTheme(
        <SecretField
          value="{{secret.prod_api_key}}"
          onChange={() => {}}
          label="API key"
          name="credential_secret"
          secrets={{ options: secretOptions }}
        />,
      );
      expect(getByRole('combobox')).toHaveAttribute('id', 'credential_secret');
    });

    it('lists the given secret options and reports a selection', async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      const { getByRole, findByRole } = renderWithTheme(
        <SecretField
          value="{{secret.prod_api_key}}"
          onChange={onChange}
          label="API key"
          secrets={{ options: secretOptions }}
        />,
      );
      await user.click(getByRole('combobox'));
      const option = await findByRole('option', { name: 'Staging API key' });
      await user.click(option);
      expect(onChange).toHaveBeenCalledWith('{{secret.staging_api_key}}');
    });

    it('hides the "create new secret" entry when canCreate/onCreate are not both supplied', async () => {
      const user = userEvent.setup();
      const { getByRole, queryByRole } = renderWithTheme(
        <SecretField
          value="{{secret.prod_api_key}}"
          onChange={() => {}}
          label="API key"
          secrets={{ options: secretOptions, canCreate: true }}
        />,
      );
      await user.click(getByRole('combobox'));
      expect(queryByRole('option', { name: 'Create new secret' })).not.toBeInTheDocument();
    });

    it('calls onCreate (not onChange) when "create new secret" is selected, using the default createLabel', async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      const onCreate = vi.fn();
      const { getByRole, findByRole } = renderWithTheme(
        <SecretField
          value="{{secret.prod_api_key}}"
          onChange={onChange}
          label="API key"
          secrets={{ options: secretOptions, canCreate: true, onCreate }}
        />,
      );
      await user.click(getByRole('combobox'));
      const createOption = await findByRole('option', { name: 'Create new secret' });
      await user.click(createOption);
      expect(onCreate).toHaveBeenCalledTimes(1);
      expect(onChange).not.toHaveBeenCalled();
    });

    it('keeps the dropdown open after "create new secret" is selected (issue 926/ELITEA-1071)', async () => {
      const user = userEvent.setup();
      const onCreate = vi.fn();
      const { getByRole, findByRole } = renderWithTheme(
        <SecretField
          value="{{secret.prod_api_key}}"
          onChange={() => {}}
          label="API key"
          secrets={{ options: secretOptions, canCreate: true, onCreate }}
        />,
      );
      await user.click(getByRole('combobox'));
      const createOption = await findByRole('option', { name: 'Create new secret' });
      await user.click(createOption);
      expect(onCreate).toHaveBeenCalledTimes(1);
      // Unlike selecting a real secret (which closes the popup, tested
      // above), the create shortcut's own `onClose` request is swallowed
      // once: the option is still findable.
      expect(await findByRole('option', { name: 'Create new secret' })).toBeInTheDocument();
    });

    it('closes the dropdown normally when a real secret is selected (unlike the create shortcut)', async () => {
      const user = userEvent.setup();
      const onChange = vi.fn();
      const { getByRole, findByRole, queryByRole } = renderWithTheme(
        <SecretField
          value="{{secret.prod_api_key}}"
          onChange={onChange}
          label="API key"
          secrets={{ options: secretOptions }}
        />,
      );
      await user.click(getByRole('combobox'));
      const option = await findByRole('option', { name: 'Staging API key' });
      await user.click(option);
      expect(queryByRole('listbox')).not.toBeInTheDocument();
    });

    it('calls onRefresh when the refresh button is clicked', async () => {
      const user = userEvent.setup();
      const onRefresh = vi.fn();
      const { getByRole } = renderWithTheme(
        <SecretField
          value="{{secret.prod_api_key}}"
          onChange={() => {}}
          label="API key"
          secrets={{ options: secretOptions, onRefresh }}
        />,
      );
      await user.click(getByRole('button', { name: 'Refresh secrets' }));
      expect(onRefresh).toHaveBeenCalledTimes(1);
    });

    it('hides the mode toggle entirely when disableToggle is set', () => {
      const { queryByRole } = renderWithTheme(
        <SecretField
          value="{{secret.prod_api_key}}"
          onChange={() => {}}
          label="API key"
          secrets={{ options: secretOptions, disableToggle: true }}
        />,
      );
      expect(queryByRole('group', { name: 'Value type' })).not.toBeInTheDocument();
    });

    it('supports custom tab labels', () => {
      const { getByRole } = renderWithTheme(
        <SecretField
          value=""
          onChange={() => {}}
          label="API key"
          secrets={{ options: secretOptions, tabLabels: { secret: 'Vault', password: 'Raw' } }}
        />,
      );
      expect(getByRole('button', { name: 'Vault' })).toBeInTheDocument();
      expect(getByRole('button', { name: 'Raw' })).toBeInTheDocument();
    });
  });
});
