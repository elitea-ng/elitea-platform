/**
 * What embedding with the deployment would take, before the person agrees to
 * it and while it runs: the tokens (an estimate, a token per four characters),
 * the estimated cost when the catalogue prices the model, what is left of the
 * bound project's budget, and the two caps a run is held to (editable).
 */
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { IndexEstimate } from '@/shared/desktop/indexIpc';
import { t } from '@/shared/i18n';

import type { WorkspaceEmbeddings } from '../model/useEmbeddings';

const count = (value: number): string => new Intl.NumberFormat().format(value);

/** `$0.04`, `under $0.01`. */
function usd(value: number): string {
  if (value > 0 && value < 0.01) return t('workspace.index.estimate.underCent', 'under $0.01');
  return `$${value.toFixed(2)}`;
}

function budgetLine(budget: NonNullable<IndexEstimate['project_budget']>): string {
  if (budget.unlimited) return t('workspace.index.estimate.budgetUnlimited', 'The project has no budget limit.');
  const percent = budget.percent_used === null ? null : String(Math.round(budget.percent_used));
  if (budget.remaining_usd !== null && percent !== null) {
    return t('workspace.index.estimate.budgetLeft', 'Project budget left this month: {{left}} ({{percent}}% used).', { left: usd(budget.remaining_usd), percent });
  }
  if (percent !== null) return t('workspace.index.estimate.budgetPercent', '{{percent}}% of the project’s monthly budget is used.', { percent });
  return t('workspace.index.estimate.budgetUnknown', 'The project’s budget is not shown to you.');
}

/** Whole tokens, 1 to 10 billion (the host's range); `null` for anything else. */
function parseCap(text: string): number | null {
  const value = Number(text);
  return Number.isInteger(value) && value >= 1 && value <= 10_000_000_000 ? value : null;
}

function Caps({ estimate, embeddings }: { estimate: IndexEstimate; embeddings: WorkspaceEmbeddings }): React.JSX.Element {
  const [run, setRun] = useState(String(estimate.caps.run));
  const [day, setDay] = useState(String(estimate.caps.day));
  const [seen, setSeen] = useState(estimate.caps);
  // The host's caps changed (saved, or another window): the fields follow.
  if (seen.run !== estimate.caps.run || seen.day !== estimate.caps.day) {
    setSeen(estimate.caps);
    setRun(String(estimate.caps.run));
    setDay(String(estimate.caps.day));
  }
  const [parsedRun, parsedDay] = [parseCap(run), parseCap(day)];
  const changed = parsedRun !== estimate.caps.run || parsedDay !== estimate.caps.day;
  return (
    <Box data-testid="index-embedding-caps" sx={{ display: 'flex', gap: 1, alignItems: 'flex-start', flexWrap: 'wrap' }}>
      <TextField
        size="small"
        label={t('workspace.index.estimate.runCap', 'Tokens per run')}
        value={run}
        error={parsedRun === null}
        onChange={(event) => setRun(event.target.value.trim())}
        slotProps={{ htmlInput: { inputMode: 'numeric' } }}
      />
      <TextField
        size="small"
        label={t('workspace.index.estimate.dayCap', 'Tokens per day')}
        value={day}
        error={parsedDay === null}
        onChange={(event) => setDay(event.target.value.trim())}
        slotProps={{ htmlInput: { inputMode: 'numeric' } }}
      />
      <Button
        size="small"
        variant="outlined"
        sx={{ marginTop: 0.5 }}
        disabled={embeddings.pending || parsedRun === null || parsedDay === null || !changed}
        onClick={() => {
          if (parsedRun !== null && parsedDay !== null) embeddings.run({ kind: 'setCaps', run: parsedRun, day: parsedDay });
        }}
      >
        {t('workspace.index.estimate.saveCaps', 'Save limits')}
      </Button>
    </Box>
  );
}

export function EmbeddingEstimate({ embeddings }: { embeddings: WorkspaceEmbeddings }): React.JSX.Element | null {
  const { estimate } = embeddings;
  if (estimate?.origin !== 'remote') return null;
  const overBudget = estimate.cost_usd !== null && estimate.project_budget?.remaining_usd != null && estimate.cost_usd > estimate.project_budget.remaining_usd;
  return (
    <Box data-testid="index-embedding-estimate" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <Typography variant="bodySmall">
        {t('workspace.index.estimate.tokens', 'About {{tokens}} tokens to embed ({{texts}} chunks). This is an estimate.', { tokens: count(estimate.tokens), texts: count(estimate.texts) })}{' '}
        {estimate.cost_usd !== null && t('workspace.index.estimate.cost', 'Estimated cost: about {{cost}}.', { cost: usd(estimate.cost_usd) })}
      </Typography>
      {estimate.project_budget !== null && <Typography variant="bodySmall">{budgetLine(estimate.project_budget)}</Typography>}
      {overBudget && (
        <Alert severity="warning">{t('workspace.index.estimate.overBudget', 'The estimate is more than the project has left this month. A run stops when the budget is used up.')}</Alert>
      )}
      <Typography variant="bodySmall">
        {t('workspace.index.estimate.limits', 'A run stops at {{run}} tokens and a day at {{day}} ({{used}} used today); this folder needs about {{days}} day(s) at that pace.', {
          run: count(estimate.caps.run),
          day: count(estimate.caps.day),
          used: count(estimate.day_used),
          days: String(estimate.days_needed ?? 1),
        })}
      </Typography>
      <Caps estimate={estimate} embeddings={embeddings} />
    </Box>
  );
}
