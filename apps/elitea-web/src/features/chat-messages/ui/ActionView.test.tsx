/**
 * The tool pin's PARTIAL OUTPUT state (#956).
 *
 * A tool result larger than one output frame arrives as chunks. When one of
 * them is lost — or the assembly does not match the digest its producer
 * stamped — the pin holds a PREFIX, and a prefix of a document reads exactly
 * like a complete short one. The row has to say which it is; that is the whole
 * reason the reducer carries the flag this file renders.
 */
import { describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ActionView } from './ActionView';

describe('ActionView', () => {
  it('says so when a chunked tool output did not arrive whole', () => {
    renderWithTheme(
      <ActionView
        action={{ name: 'read_file', toolOutputs: 'the first 30k of it…', toolOutputPartial: true }}
      />,
    );

    expect(screen.getByTestId('chat-tool-action-partial-output')).toHaveTextContent(/partial output/i);
  });

  it('says nothing of the kind for an ordinary complete result', () => {
    renderWithTheme(<ActionView action={{ name: 'read_file', toolOutputs: 'a short result' }} />);

    expect(screen.queryByTestId('chat-tool-action-partial-output')).toBeNull();
    expect(screen.getByTestId('chat-tool-action')).toHaveTextContent('a short result');
  });

  it('says nothing of the kind for a chunked output that DID arrive whole', () => {
    renderWithTheme(
      <ActionView action={{ name: 'read_file', toolOutputs: 'all of it', toolOutputPartial: false }} />,
    );

    expect(screen.queryByTestId('chat-tool-action-partial-output')).toBeNull();
  });
});
