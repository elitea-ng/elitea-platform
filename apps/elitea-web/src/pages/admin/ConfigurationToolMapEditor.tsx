/**
 * The editor behind the Guardrails section's two map fields — `blocked_tools`
 * and `sensitive_tools`, both `{toolkit type: [tool names]}`.
 *
 * A row per toolkit, each row carrying a multi-select of that toolkit's tools.
 * Both inputs are FREE-SOLO on purpose, and the reason is not convenience:
 *
 *   - the toolkit key is not always a toolkit name. `sensitive_tools` accepts
 *     `*`, which applies to every toolkit and is the shape the shipped compose
 *     files already use;
 *   - `blocked_tools` legitimately names types the pinned SDK snapshot does not
 *     declare — the four elitea_core-native ones, and anything a newer SDK adds.
 *     A closed picker would make an existing, working entry unrepresentable.
 *
 * So the registry supplies SUGGESTIONS and never a whitelist. A deployment whose
 * registry cannot be enumerated (the endpoint answers 501) gets plain text
 * boxes and full function, rather than an empty dropdown and none.
 *
 * ## Matching is case- and separator-insensitive, and the operator is told so
 *
 * `Create File`, `create_file` and `create-file` are one entry to the guardrail
 * (`internal/domain/guardrails`), so two rows that look different can be the
 * same rule. The editor surfaces a duplicate rather than silently letting the
 * later row win on save — a JSON object cannot hold the same key twice, so one
 * of them WOULD be discarded, and discarding an operator's rule without saying
 * so is the failure this whole page exists to remove.
 */
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlineOutlined';
import Autocomplete from '@mui/material/Autocomplete';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { useToolkitToolSuggestions } from './api/adminConfigurationApi';

/** One editable row. Order is the editor's, not the value's — see toConfigToolMapRows. */
export interface ConfigToolMapRow {
  readonly toolkit: string;
  readonly tools: readonly string[];
}

/**
 * The comparison key the server and the worker both use: lowercase, then drop
 * every character that is not an ASCII letter or digit, preserving `*`.
 *
 * Duplicated here rather than imported from `features/agents/lib/toolkitBlocklist`
 * because that slice is a feature and this is a page — importing sideways is
 * exactly what the app's boundary rule forbids. It is nine characters of regex
 * and the Go side's `CanonicalKey` is the authority; this copy only decides
 * whether to show a warning, never what gets stored.
 */
export function canonicalConfigKey(value: string): string {
  const trimmed = value.trim();
  if (trimmed === '*') return '*';
  return trimmed.toLowerCase().replace(/[^a-z0-9]/g, '');
}

/**
 * Reads a stored value into rows.
 *
 * Sorted by toolkit key so the list does not reshuffle between loads — a JSON
 * object has no order, and `Object.entries` follows insertion order, which for a
 * value that came back from Postgres is whatever the row scan produced.
 */
export function toConfigToolMapRows(value: unknown): ConfigToolMapRow[] {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return [];
  return Object.entries(value as Record<string, unknown>)
    .map(([toolkit, tools]) => ({
      toolkit,
      tools: Array.isArray(tools) ? tools.filter((tool): tool is string => typeof tool === 'string') : [],
    }))
    .sort((left, right) => left.toolkit.localeCompare(right.toolkit));
}

/**
 * Turns rows back into the value the field holds.
 *
 * A row with a blank toolkit is dropped: it is a row the operator has started
 * and not named, and storing `""` would be a key the guardrail canonicalises to
 * nothing and ignores. A row with a toolkit and NO tools is KEPT — that is a
 * meaningful, if inert, statement ("I have selected this toolkit, and no tools
 * yet"), and dropping it would delete the row the operator is halfway through
 * filling in on the keystroke that named it.
 */
export function fromConfigToolMapRows(rows: readonly ConfigToolMapRow[]): Record<string, string[]> {
  const value: Record<string, string[]> = {};
  for (const row of rows) {
    const toolkit = row.toolkit.trim();
    if (toolkit === '') continue;
    value[toolkit] = row.tools.map((tool) => tool.trim()).filter((tool) => tool !== '');
  }
  return value;
}

/**
 * The rows whose toolkit key collides with an earlier row's, by INDEX.
 *
 * By index rather than by key because the warning belongs on the second
 * occurrence — the one that will be lost — and two rows can collide while
 * looking different (`GitHub` and `git_hub`).
 */
export function duplicateToolkitRows(rows: readonly ConfigToolMapRow[]): ReadonlySet<number> {
  const seen = new Map<string, number>();
  const duplicates = new Set<number>();
  rows.forEach((row, index) => {
    const key = canonicalConfigKey(row.toolkit);
    if (key === '') return;
    if (seen.has(key)) {
      duplicates.add(index);
      return;
    }
    seen.set(key, index);
  });
  return duplicates;
}

interface RowProps {
  readonly row: ConfigToolMapRow;
  readonly duplicate: boolean;
  readonly disabled: boolean;
  readonly toolkitOptions: readonly string[];
  readonly toolSource: string | undefined;
  readonly onChange: (next: ConfigToolMapRow) => void;
  readonly onRemove: () => void;
}

function ToolMapRow({
  row,
  duplicate,
  disabled,
  toolkitOptions,
  toolSource,
  onChange,
  onRemove,
}: RowProps) {
  // Scoped to THIS row's toolkit, and re-fetched when it changes. The hook is
  // disabled for a blank key and for `*`, so a wildcard row offers no tool
  // suggestions — there is no single toolkit whose list would be the right one.
  const toolOptions = useToolkitToolSuggestions(toolSource, row.toolkit);

  return (
    <Box sx={{ display: 'flex', gap: '0.75rem', alignItems: 'flex-start' }}>
      <Autocomplete
        freeSolo
        disabled={disabled}
        options={[...toolkitOptions]}
        value={row.toolkit}
        sx={{ flex: '0 0 14rem' }}
        onInputChange={(_event, next) => {
          onChange({ toolkit: next, tools: row.tools });
        }}
        renderInput={(params) => (
          <TextField
            {...params}
            size="small"
            label={t('pages.admin.configuration.toolMap.toolkit', 'Toolkit type')}
            error={duplicate}
            helperText={
              duplicate
                ? t(
                    'pages.admin.configuration.toolMap.duplicate',
                    'Another row already covers this toolkit. Matching ignores case and separators, so only one of them would be saved.',
                  )
                : undefined
            }
          />
        )}
      />
      <Autocomplete
        multiple
        freeSolo
        disabled={disabled}
        options={[...toolOptions]}
        value={[...row.tools]}
        sx={{ flex: 1 }}
        onChange={(_event, next) => {
          onChange({ toolkit: row.toolkit, tools: next });
        }}
        renderInput={(params) => (
          <TextField
            {...params}
            size="small"
            label={t('pages.admin.configuration.toolMap.tools', 'Tools')}
            placeholder={t('pages.admin.configuration.toolMap.toolsPlaceholder', 'Add a tool name')}
          />
        )}
      />
      <IconButton
        aria-label={t('pages.admin.configuration.toolMap.remove', 'Remove this toolkit')}
        disabled={disabled}
        onClick={onRemove}
        sx={{ mt: '0.25rem' }}
      >
        <DeleteOutlineIcon fontSize="small" />
      </IconButton>
    </Box>
  );
}

export function ConfigurationToolMapEditor({
  label,
  rows,
  disabled,
  toolkitOptions,
  toolSource,
  onChange,
}: {
  /**
   * The field's title, appended to the add button.
   *
   * Not decoration. Guardrails renders TWO of these editors on one screen, so a
   * bare "Add toolkit" would give the page two buttons with the same accessible
   * name and no way to tell which map they belong to. The array editor beside
   * them already names its field for the same reason.
   */
  readonly label: string;
  readonly rows: readonly ConfigToolMapRow[];
  readonly disabled: boolean;
  readonly toolkitOptions: readonly string[];
  readonly toolSource: string | undefined;
  readonly onChange: (next: readonly ConfigToolMapRow[]) => void;
}) {
  const duplicates = duplicateToolkitRows(rows);

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      {rows.length === 0 ? (
        <Typography variant="bodyMedium" component="p" color="text.secondary">
          {t('pages.admin.configuration.toolMap.empty', 'No toolkits are listed.')}
        </Typography>
      ) : (
        rows.map((row, index) => (
          <ToolMapRow
            // The index IS the identity here: rows have no id, and a blank
            // toolkit is a legitimate in-progress state, so keying on the
            // toolkit would remount the row on every keystroke and lose focus.
            // eslint-disable-next-line react/no-array-index-key
            key={index}
            row={row}
            duplicate={duplicates.has(index)}
            disabled={disabled}
            toolkitOptions={toolkitOptions}
            toolSource={toolSource}
            onChange={(next) => {
              onChange(rows.map((current, position) => (position === index ? next : current)));
            }}
            onRemove={() => {
              onChange(rows.filter((_current, position) => position !== index));
            }}
          />
        ))
      )}
      <Box>
        <Button
            variant="elitea" color="tertiary"
          size="small"
          disabled={disabled}
          onClick={() => {
            onChange([...rows, { toolkit: '', tools: [] }]);
          }}
        >
          {`${t('pages.admin.configuration.toolMap.add', 'Add toolkit')} — ${label}`}
        </Button>
      </Box>
    </Box>
  );
}
