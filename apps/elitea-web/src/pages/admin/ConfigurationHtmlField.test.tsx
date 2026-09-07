/**
 * The `format: "html"` editor — today, the maintenance splash body.
 *
 * The preview is what these tests are about. An operator authoring markup that
 * lands on the screen of every user a maintenance window refuses has to see two
 * things before they save: what it will look like, and what the platform will
 * strip. The two must agree with the live splash, which is why the preview and
 * the splash share one sanitiser.
 */
import { configure, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { ConfigurationHtmlField } from './ConfigurationHtmlField';
import { widgetFor } from './configurationFields';

configure({ asyncUtilTimeout: 5_000 });
vi.setConfig({ testTimeout: 30_000 });

const FIELD = {
  key: 'maintenance_html',
  type: 'string',
  format: 'html',
  title: 'Splash HTML',
  description: 'Optional HTML body for the splash screen.',
} as const;

function renderField(value: string) {
  const onChange = vi.fn();
  render(
    <ConfigurationHtmlField field={FIELD} value={value} disabled={false} onChange={onChange} />,
  );
  return { onChange };
}

describe('widgetFor', () => {
  it('routes a format:html string to the HTML editor, not to the plain textarea', () => {
    expect(widgetFor(FIELD)).toBe('html');
    expect(widgetFor({ key: 'maintenance_message', type: 'string', title: '', format: 'textarea' })).toBe(
      'multiline',
    );
  });
});

describe('ConfigurationHtmlField', () => {
  it('says nothing is being previewed while the field is empty', () => {
    renderField('');
    expect(screen.getByTestId('admin-config-html-preview-maintenance_html')).toHaveTextContent(
      /Nothing to preview/,
    );
  });

  it('renders the markup the operator typed', async () => {
    renderField('<p>Back at <b>14:00 UTC</b>.</p>');
    const preview = screen.getByTestId('admin-config-html-preview-maintenance_html');
    expect(preview).toHaveTextContent('Back at 14:00 UTC.');
    expect(preview.querySelector('b')).not.toBeNull();
    // Nothing was removed, so the warning does not appear.
    await waitFor(() => {
      expect(screen.queryByTestId('admin-config-html-stripped-maintenance_html')).toBeNull();
    });
  });

  it('previews the SANITISED markup and says so when something was removed', () => {
    renderField('<p>Back soon.</p><script>window.pwned = true</script>');
    const preview = screen.getByTestId('admin-config-html-preview-maintenance_html');
    expect(preview).toHaveTextContent('Back soon.');
    expect(preview.querySelector('script')).toBeNull();
    expect(preview.innerHTML).not.toContain('pwned');
    expect(screen.getByTestId('admin-config-html-stripped-maintenance_html')).toBeVisible();
  });

  it('reports every keystroke under the field key, so the form holds one source of truth', async () => {
    const { onChange } = renderField('');
    await userEvent.type(screen.getByTestId('admin-config-html-maintenance_html').querySelector('textarea')!, 'a');
    expect(onChange).toHaveBeenCalledWith('maintenance_html', 'a');
  });
});
