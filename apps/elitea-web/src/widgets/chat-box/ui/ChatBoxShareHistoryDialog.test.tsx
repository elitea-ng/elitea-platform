import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { ChatBoxShareHistoryDialog } from './ChatBoxShareHistoryDialog';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderDialog(open: boolean) {
  const question = { open, onChoose: vi.fn(), onCancel: vi.fn() };
  render(
    <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
      <ChatBoxShareHistoryDialog question={question} />
    </ThemeProvider>,
  );
  return question;
}

describe('ChatBoxShareHistoryDialog', () => {
  it('renders nothing until an agent is added to a conversation with messages', () => {
    renderDialog(false);
    expect(screen.queryByTestId('share-history-dialog')).not.toBeInTheDocument();
  });

  it('answers "now" and "the start"', () => {
    const question = renderDialog(true);
    fireEvent.click(screen.getByTestId('share-history-now'));
    expect(question.onChoose).toHaveBeenCalledWith('now');
    fireEvent.click(screen.getByTestId('share-history-start'));
    expect(question.onChoose).toHaveBeenCalledWith('start');
  });
});
