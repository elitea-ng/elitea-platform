/**
 * `renderPipelineCreateForm` is the pipeline editor's create-mode form body
 * for the chat composition root — before it existed, "Create new" under the
 * composer's Pipelines submenu opened an editor with no form at all (see
 * this module's own doc comment). The only claim worth making here is that
 * it renders `CreateAgentForm` as `entityType="pipeline"` /
 * `showInstructions={false}` and wires `values`/`onFieldChange` straight
 * through — `CreateAgentForm` itself is covered exhaustively in
 * `features/agents`.
 */
import { render, screen } from '@testing-library/react';
import { ThemeProvider } from '@mui/material/styles';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { renderPipelineCreateForm } from './PipelineCreateForm';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

describe('renderPipelineCreateForm', () => {
  it('renders the shared create form, seeded from the given draft values', () => {
    const onFieldChange = vi.fn();
    render(
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        {renderPipelineCreateForm({
          values: { name: 'My pipeline', description: 'Does things', version_details: { welcome_message: 'Hi' } },
          onFieldChange,
        })}
      </ThemeProvider>,
    );

    expect(screen.getByDisplayValue('My pipeline')).toBeInTheDocument();
    expect(screen.getByDisplayValue('Does things')).toBeInTheDocument();
    // `showInstructions={false}` — the free-text instructions field this
    // module's own doc comment says must not appear at create time.
    expect(screen.queryByLabelText(/instructions/i)).not.toBeInTheDocument();
  });
});
