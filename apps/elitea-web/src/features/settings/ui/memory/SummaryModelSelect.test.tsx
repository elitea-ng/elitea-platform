import { Form, Formik } from 'formik';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { deserializeMemoryBlocks, serializeSettingsProfile } from '../ai-personality/settingsProfileForm';
import { SummaryModelSelect } from './SummaryModelSelect';

describe('SummaryModelSelect', () => {
  it('distinguishes the same model name in different projects and can restore the chat model', async () => {
    const submit = vi.fn<(value: ReturnType<typeof deserializeMemoryBlocks>) => void>();
    const user = userEvent.setup();
    renderWithTheme(<Formik initialValues={serializeSettingsProfile(undefined, '2')} onSubmit={(values) => { submit(deserializeMemoryBlocks(values)); }}>
      <Form>
        <SummaryModelSelect models={[{ name: 'summary', project_id: '2' }, { name: 'summary', project_id: '7' }]} />
        <button type="submit">Save</button>
      </Form>
    </Formik>);
    await user.click(screen.getByRole('combobox', { name: 'Summarization model' }));
    await user.click(screen.getByRole('option', { name: 'summary (7)' }));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
    expect(submit.mock.calls[0]?.[0].default_summarization.summary_model_name).toBe('summary');
    expect(submit.mock.calls[0]?.[0].default_summarization.summary_model_project_id).toBe(7);
    await user.click(screen.getByRole('combobox', { name: 'Summarization model' }));
    await user.click(screen.getByRole('option', { name: 'Use the chat model' }));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(submit).toHaveBeenCalledTimes(2));
    expect(submit.mock.calls[1]?.[0].default_summarization).not.toHaveProperty('summary_model_project_id');
  });
});
