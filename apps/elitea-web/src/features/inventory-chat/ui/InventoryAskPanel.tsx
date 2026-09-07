/**
 * The ask panel: a question, what the agent did, and the answer with its
 * citations.
 *
 * THE STEPS ARE SHOWN WHILE THE QUESTION RUNS AND DROPPED WHEN IT LANDS. They
 * are the evidence that something is happening during a wait that is measured
 * in tens of seconds; once the answer is there, they are noise beside it. The
 * legacy screen kept them behind a disclosure, which is the same decision made
 * for a rail 300 pixels wide.
 *
 * A CITATION IS A LINK. An answer a reader cannot check against the graph it
 * came from is an answer they have to trust; clicking an id opens that entity
 * in the browser, which is what makes the answer falsifiable.
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import Link from '@mui/material/Link';
import Stack from '@mui/material/Stack';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import type { AskTurn } from '../model/useInventoryAsk';

const panelSx = { display: 'flex', flexDirection: 'column', gap: 1.5, height: '100%' } as const;
const transcriptSx = { flex: 1, minHeight: 0, overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: 2 } as const;
const composerSx = { display: 'flex', alignItems: 'flex-start', gap: 1 } as const;
const headerSx = { display: 'flex', justifyContent: 'flex-end' } as const;
const answerSx = { whiteSpace: 'pre-wrap' } as const;
const chipRowSx = { display: 'flex', flexWrap: 'wrap', gap: 1 } as const;

export interface InventoryAskPanelProps {
  readonly turns: readonly AskTurn[];
  readonly pendingQuestion: string | null;
  readonly steps: readonly string[];
  readonly error: string | null;
  readonly onAsk: (question: string) => void;
  readonly onStop: () => void;
  readonly onClear: () => void;
  readonly onOpenEntity: (entityId: string) => void;
}

export function InventoryAskPanel({
  turns,
  pendingQuestion,
  steps,
  error,
  onAsk,
  onStop,
  onClear,
  onOpenEntity,
}: InventoryAskPanelProps): React.JSX.Element {
  const [draft, setDraft] = useState('');
  const running = pendingQuestion !== null;

  return (
    <Box sx={panelSx} data-testid="inventory-ask-panel">
      {turns.length === 0 ? null : (
        <Box sx={headerSx}>
          <BaseBtn variant="text" size="small" data-testid="inventory-ask-clear" onClick={onClear}>
            {t('inventory.ask.clear', 'Clear')}
          </BaseBtn>
        </Box>
      )}

      <Box sx={transcriptSx} data-testid="inventory-ask-transcript">
        {turns.length === 0 && !running ? (
          <Typography variant="bodyMedium" color="text.secondary" data-testid="inventory-ask-empty">
            {t(
              'inventory.ask.empty',
              'Ask a question about this knowledge graph. The answer names the entities it came from.',
            )}
          </Typography>
        ) : null}

        {turns.map((turn, index) => (
          <Box key={`${index}:${turn.question}`} data-testid="inventory-ask-turn">
            <Typography variant="labelSmall" color="text.secondary">
              {turn.question}
            </Typography>
            <Typography variant="bodyMedium" sx={answerSx} data-testid="inventory-ask-answer">
              {turn.answer}
            </Typography>
            {turn.entities.length === 0 ? null : (
              <Box sx={chipRowSx} data-testid="inventory-ask-citations">
                {turn.entities.map((entityId) => (
                  <Link
                    key={entityId}
                    component="button"
                    type="button"
                    underline="hover"
                    variant="bodySmall"
                    data-testid="inventory-ask-citation"
                    data-entity-id={entityId}
                    onClick={() => {
                      onOpenEntity(entityId);
                    }}
                  >
                    {entityId}
                  </Link>
                ))}
              </Box>
            )}
          </Box>
        ))}

        {running ? (
          <Box data-testid="inventory-ask-running">
            <Typography variant="labelSmall" color="text.secondary">
              {pendingQuestion}
            </Typography>
            <Stack spacing={0.5} data-testid="inventory-ask-steps">
              {steps.length === 0 ? (
                <Typography variant="bodySmall" color="text.secondary">
                  {t('inventory.ask.thinking', 'Reading the graph…')}
                </Typography>
              ) : (
                steps.map((step, index) => (
                  <Typography key={`${index}:${step}`} variant="bodySmall" color="text.secondary">
                    {step}
                  </Typography>
                ))
              )}
            </Stack>
          </Box>
        ) : null}

        {error === null ? null : <BannerMessage variant="error" message={error} />}
      </Box>

      <Box
        component="form"
        sx={composerSx}
        onSubmit={(event) => {
          event.preventDefault();
          const question = draft.trim();
          if (question === '' || running) return;
          setDraft('');
          onAsk(question);
        }}
      >
        <TextField
          fullWidth
          multiline
          size="small"
          maxRows={4}
          value={draft}
          disabled={running}
          // ON THE INPUT, not on the TextField. An unrecognised prop passed to
          // TextField lands on the root FormControl <div>, so a test id put
          // there names an element that cannot be typed into and is never
          // disabled — a locator that resolves and does nothing, in a browser
          // journey as well as in a unit test.
          slotProps={{ htmlInput: { 'data-testid': 'inventory-ask-input' } }}
          placeholder={t('inventory.ask.placeholder', 'Ask about this inventory…')}
          onChange={(event) => {
            setDraft(event.target.value);
          }}
        />
        {running ? (
          <BaseBtn variant="alarm" size="small" data-testid="inventory-ask-stop" onClick={onStop}>
            {t('inventory.ask.stop', 'Stop')}
          </BaseBtn>
        ) : (
          <BaseBtn variant="elitea" size="small" type="submit" data-testid="inventory-ask-send">
            {t('inventory.ask.send', 'Ask')}
          </BaseBtn>
        )}
      </Box>
    </Box>
  );
}
