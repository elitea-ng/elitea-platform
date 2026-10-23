/**
 * What embedding with the deployment would take, before the person agrees to
 * it and while it runs: the tokens (an estimate, a token per four characters),
 * the estimated cost when the catalogue prices the model (the price comes with
 * the project's model list the picker already read, in the catalogue's
 * currency), what is left of the bound project's budget (in its currency), and
 * the two caps a run is held to (editable).
 */
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { IndexEstimate, RemoteModel } from '@/shared/desktop/indexIpc';
import { t } from '@/shared/i18n';
import { formatMoney } from '@/shared/lib/money';

import type { WorkspaceEmbeddings } from '../model/useEmbeddings';

const count = (value: number): string => new Intl.NumberFormat().format(value);

function budgetLine(budget: NonNullable<IndexEstimate['project_budget']>): string {
  if (budget.unlimited) return t('workspace.index.estimate.budgetUnlimited', 'The project has no budget limit.');
  const percent = budget.percent_used === null ? null : String(Math.round(budget.percent_used));
  if (budget.remaining !== null && budget.currency !== null && percent !== null) {
    return t('workspace.index.estimate.budgetLeft', 'Project budget left this month: {{left}} ({{percent}}% used).', { left: formatMoney(budget.remaining, budget.currency), percent });
  }
  if (percent !== null) return t('workspace.index.estimate.budgetPercent', '{{percent}}% of the project’s monthly budget is used.', { percent });
  return t('workspace.index.estimate.budgetUnknown', 'The project’s budget is not shown to you.');
}

interface Cost {
  amount: number;
  currency: string;
}

/** The estimate's tokens at the model's price from the picker's list; `null` when the catalogue prices it not. */
function costOf(estimate: IndexEstimate, models: readonly RemoteModel[] | undefined): Cost | null {
  const priced = models?.find((model) => model.name === estimate.model);
  if (priced?.price_per_1m == null || priced.currency === null) return null;
  return { amount: (estimate.tokens / 1_000_000) * priced.price_per_1m, currency: priced.currency };
}

/** Whether the cost is more than the budget has left (compared only in one currency). */
function isOverBudget(cost: Cost | null, budget: IndexEstimate['project_budget']): boolean {
  return cost !== null && budget?.remaining != null && budget.currency === cost.currency && cost.amount > budget.remaining;
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
  const cost = costOf(estimate, embeddings.remoteModels);
  const budget = estimate.project_budget;
  const overBudget = isOverBudget(cost, budget);
  return (
    <Box data-testid="index-embedding-estimate" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
      <Typography variant="bodySmall">
        {t('workspace.index.estimate.tokens', 'About {{tokens}} tokens to embed ({{texts}} chunks). This is an estimate.', { tokens: count(estimate.tokens), texts: count(estimate.texts) })}{' '}
        {cost !== null && t('workspace.index.estimate.cost', 'Estimated cost: about {{cost}}.', { cost: formatMoney(cost.amount, cost.currency) })}
      </Typography>
      {budget !== null && <Typography variant="bodySmall">{budgetLine(budget)}</Typography>}
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
