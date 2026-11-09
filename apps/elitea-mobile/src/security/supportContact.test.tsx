// The IT support line (ADR-0052 W6.2): managed beats workspace, and the rows show it.
import { fireEvent, render, screen } from '@testing-library/react-native';

import { parseManagedConfig } from '@/security/managedConfig';
import { applyServerPolicy, policyFrom } from '@/security/policyStore';
import { getPolicy } from '@/security/policy';
import { supportContactFrom } from '@/security/supportContact';
import { AboutView } from '@/features/settings/AboutView';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

test('managed beats the token policy, which beats discovery', () => {
  expect(supportContactFrom('Managed desk, help@example.com', 'Workspace desk', 'Public desk')).toEqual({
    text: 'Managed desk, help@example.com',
    href: 'mailto:help@example.com',
  });
  expect(supportContactFrom(null, 'Workspace desk', 'Public desk')?.text).toBe('Workspace desk');
  expect(supportContactFrom(null, null, 'Public desk, +1 555 010 0100')).toEqual({ text: 'Public desk, +1 555 010 0100', href: 'tel:+15550100100' });
  // Dormant: a server before 1.45 and no MDM.
  expect(supportContactFrom(null, null, undefined)).toBeNull();
});

test('the managed key: one line, trimmed; anything else is ignored', () => {
  expect(parseManagedConfig({ support_contact: '  IT desk ' }).supportContact).toBe('IT desk');
  expect(parseManagedConfig({ support_contact: 'IT\ndesk' }).supportContact).toBeNull();
  expect(parseManagedConfig({ support_contact: 5 }).supportContact).toBeNull();
  // An ignored support line keeps the rest of the configuration.
  expect(parseManagedConfig({ support_contact: '', lock_server: true }).lockServer).toBe(true);
});

test('the workspace policy carries it, normalized', async () => {
  expect(policyFrom({ support_contact: ' IT desk ' }).support_contact).toBe('IT desk');
  expect(policyFrom({ support_contact: 'x'.repeat(201) }).support_contact).toBeUndefined();
  await applyServerPolicy({ support_contact: 'IT desk' }, { persist: false });
  expect(getPolicy().support_contact).toBe('IT desk');
  await applyServerPolicy({}, { persist: false });
  expect(getPolicy().support_contact).toBeNull();
});

test('About shows the line, opens it only when it is a link', async () => {
  const onSupportContact = jest.fn();
  const { rerender } = await render(
    <AboutView
      version="1.0.0"
      onPrivacy={jest.fn()}
      onSupport={jest.fn()}
      supportContact={{ text: 'IT desk, help@example.com', href: 'mailto:help@example.com' }}
      onSupportContact={onSupportContact}
    />,
  );
  expect(screen.getByText('IT support')).toBeTruthy();
  await fireEvent.press(screen.getByText('IT desk, help@example.com'));
  expect(onSupportContact).toHaveBeenCalledTimes(1);

  await rerender(<AboutView version="1.0.0" onPrivacy={jest.fn()} onSupport={jest.fn()} supportContact={{ text: 'Floor 3', href: null }} onSupportContact={onSupportContact} />);
  // Text to read: not a button.
  expect(screen.getByText('Floor 3')).toBeTruthy();
  expect(screen.queryByRole('button', { name: /IT support/ })).toBeNull();

  await rerender(<AboutView version="1.0.0" onPrivacy={jest.fn()} onSupport={jest.fn()} />);
  expect(screen.queryByText('IT support')).toBeNull();
});
