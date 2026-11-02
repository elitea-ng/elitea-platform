/**
 * Helpers for the self-hosted classifiers and the prompt-injection slot of a
 * content guardrail (ADR-0043 decisions 1 and 4).
 *
 * The detectors stay JSON text (GovernanceGuardrailFields.tsx). This part
 * edits that text:
 *
 * - "Add" appends a Llama Guard or a Presidio detector template.
 * - On an input row, one switch per detector puts the detector in the
 *   `prompt_injection` slot. The switch sets `content: tool_results` and
 *   `slot: prompt_injection`, and changes a `mask` action to `block`: the slot
 *   allows block and flag only. The server checks every key on write.
 */
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import FormHelperText from '@mui/material/FormHelperText';
import MenuItem from '@mui/material/MenuItem';
import Stack from '@mui/material/Stack';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import { useState } from 'react';

import { t } from '@/shared/i18n';

import { parseGuardrailDetectors } from './governanceGuardrailDraft';

type Detector = Record<string, unknown>;

const TEMPLATES: Record<string, Detector> = {
  llama_guard: {
    name: 'llama-guard',
    type: 'llama_guard',
    action: 'block',
    url: 'http://llama-guard.internal:8000/v1/chat/completions',
    model: 'meta-llama/Llama-Guard-4-12B',
  },
  presidio: {
    name: 'pii',
    type: 'presidio',
    action: 'mask',
    url: 'http://presidio-analyzer.internal:3000/analyze',
    language: 'en',
  },
};

/** The detector types that cannot fill the slot: they find data, not instructions. */
const NO_SLOT_TYPES = new Set(['secrets', 'presidio']);

function isSlot(d: Detector): boolean {
  return d.slot === 'prompt_injection';
}

function withSlot(d: Detector, on: boolean): Detector {
  const next: Detector = { ...d };
  if (on) {
    next.content = 'tool_results';
    next.slot = 'prompt_injection';
    if (next.action === 'mask') next.action = 'block';
  } else {
    delete next.slot;
    delete next.content;
  }
  return next;
}

interface SelfHostedFieldsProps {
  readonly detectors: string;
  readonly isInput: boolean;
  readonly onDetectorsChange: (detectors: string) => void;
}

/** The add-a-classifier control and the prompt-injection slot switches. */
export function GuardrailSelfHostedFields({ detectors, isInput, onDetectorsChange }: SelfHostedFieldsProps) {
  const [template, setTemplate] = useState('llama_guard');
  const parsed = parseGuardrailDetectors(detectors);
  const list = 'list' in parsed ? (parsed.list as Detector[]) : undefined;
  const write = (next: Detector[]) => onDetectorsChange(JSON.stringify(next, null, 2));

  return (
    <Stack spacing={1}>
      <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
        <TextField
          select
          label={t('pages.admin.governance.field.guardrailSelfHosted', 'Self-hosted classifier')}
          value={template}
          onChange={(event) => setTemplate(event.target.value)}
          size="small"
          sx={{ minWidth: 220 }}
          slotProps={{
            htmlInput: { 'data-testid': 'governance-guardrail-template' },
          }}
        >
          <MenuItem value="llama_guard">
            {t('pages.admin.governance.field.guardrailLlamaGuard', 'Llama Guard')}
          </MenuItem>
          <MenuItem value="presidio">
            {t('pages.admin.governance.field.guardrailPresidio', 'Presidio Analyzer')}
          </MenuItem>
        </TextField>
        <Button
          variant="outlined"
          size="small"
          disabled={list === undefined}
          onClick={() => list && write([...list, { ...TEMPLATES[template] }])}
          data-testid="governance-guardrail-add-template"
        >
          {t('pages.admin.governance.field.guardrailAddDetector', 'Add detector')}
        </Button>
      </Stack>
      {isInput && list !== undefined && list.length > 0 ? (
        <>
          <FormHelperText>
            {t(
              'pages.admin.governance.field.guardrailSlotHelp',
              'Prompt-injection slot: the detector reads tool results only, and every finding gets the label prompt_injection. The slot allows block and flag.',
            )}
          </FormHelperText>
          {list.map((d, i) => {
            const type = typeof d.type === 'string' ? d.type : '';
            const name = typeof d.name === 'string' && d.name !== '' ? d.name : type;
            return (
              <FormControlLabel
                key={`${i}-${name}`}
                control={
                  <Switch
                    checked={isSlot(d)}
                    disabled={NO_SLOT_TYPES.has(type)}
                    onChange={(event) => write(list.map((x, j) => (j === i ? withSlot(x, event.target.checked) : x)))}
                    data-testid={`governance-guardrail-slot-${i}`}
                  />
                }
                label={t('pages.admin.governance.field.guardrailSlotToggle', '{{name}}: prompt-injection slot', {
                  name,
                })}
              />
            );
          })}
        </>
      ) : null}
    </Stack>
  );
}
