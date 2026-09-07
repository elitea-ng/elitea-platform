/**
 * Choose what a wiki is generated FROM: a repository, or a folder in this
 * project's artifact store.
 *
 * THE JSON STAYS THE RECORD. This control writes nothing itself — it reports
 * the folder it was given, and `WikiSettingsPanel` puts it into the draft
 * document. So the Save button, the validator and the saved settings are the
 * same ones a hand-typed `artifact_configuration` goes through, and there is
 * no second path into the toolkit for the picker to disagree with.
 *
 * THE FIELDS ARE SEEDED ONCE, from the settings as loaded, and then belong to
 * this component. A control that re-read the draft on every keystroke could
 * not hold a half-typed folder: the reader canonicalises `docs/` to `docs`,
 * so the next slash would be taken back out of the box as it was typed.
 *
 * The buckets come from the artifacts feature's own hook. It is the project's
 * bucket list, already filtered of the system buckets, and a second fetch of
 * the same route here would be a second answer to disagree with.
 */
import { useState } from 'react';

import FormControl from '@mui/material/FormControl';
import InputLabel from '@mui/material/InputLabel';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import Stack from '@mui/material/Stack';
import TextField from '@mui/material/TextField';
import ToggleButton from '@mui/material/ToggleButton';
import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';

import { useArtifactBuckets } from '@/features/artifacts';
import { t } from '@/shared/i18n';

/** One folder: a bucket in this project, and a prefix that may be empty. */
export interface WikiFolderSource {
  readonly bucket: string;
  readonly prefix: string;
}

type SourceKind = 'repository' | 'folder';

interface WikiSourcePickerProps {
  readonly projectId: string | number;
  /** The folder the draft names, as loaded. Null when it names a repository. */
  readonly source: WikiFolderSource | null;
  /** True while the draft does not parse: the picker cannot rewrite a document it cannot read. */
  readonly disabled: boolean;
  readonly onChange: (source: WikiFolderSource | null) => void;
}

export function WikiSourcePicker({ projectId, source, disabled, onChange }: WikiSourcePickerProps): React.JSX.Element {
  const [kind, setKind] = useState<SourceKind>(source === null ? 'repository' : 'folder');
  const [bucket, setBucket] = useState(source?.bucket ?? '');
  const [prefix, setPrefix] = useState(source?.prefix ?? '');
  const buckets = useArtifactBuckets(String(projectId));

  // A folder with no bucket is not a source, so it is reported as none at
  // all: the draft then names nothing, and the validator says so — which is
  // the truth about a folder the operator has not finished choosing.
  const report = (nextKind: SourceKind, nextBucket: string, nextPrefix: string): void => {
    if (nextKind === 'repository' || nextBucket === '') onChange(null);
    else onChange({ bucket: nextBucket, prefix: nextPrefix });
  };

  return (
    <Stack sx={{ flexDirection: 'row', alignItems: 'center', gap: 1, flexWrap: 'wrap' }} data-testid="wiki-source">
      <ToggleButtonGroup
        exclusive
        size="small"
        value={kind}
        disabled={disabled}
        data-testid="wiki-source-kind"
        aria-label={t('deepwiki.source.label', 'Wiki source')}
        onChange={(_event, next: SourceKind | null) => {
          if (next === null) return;
          setKind(next);
          report(next, bucket, prefix);
        }}
      >
        <ToggleButton value="repository" data-testid="wiki-source-kind-repository">
          {t('deepwiki.source.repository', 'Repository')}
        </ToggleButton>
        <ToggleButton value="folder" data-testid="wiki-source-kind-folder">
          {t('deepwiki.source.folder', 'Artifact folder')}
        </ToggleButton>
      </ToggleButtonGroup>

      {kind === 'folder' ? (
        <FormControl size="small" sx={{ minWidth: '12rem' }} disabled={disabled} data-testid="wiki-source-bucket">
          <InputLabel id="wiki-source-bucket-label">{t('deepwiki.source.bucket', 'Bucket')}</InputLabel>
          <Select
            labelId="wiki-source-bucket-label"
            label={t('deepwiki.source.bucket', 'Bucket')}
            value={bucket}
            onChange={(event) => {
              setBucket(event.target.value);
              report('folder', event.target.value, prefix);
            }}
          >
            {(buckets.data ?? []).map((row) => (
              <MenuItem key={row.id} value={row.name}>
                {row.name}
              </MenuItem>
            ))}
          </Select>
        </FormControl>
      ) : null}

      {kind === 'folder' ? (
        <TextField
          size="small"
          disabled={disabled}
          label={t('deepwiki.source.prefix', 'Folder (optional)')}
          value={prefix}
          slotProps={{ htmlInput: { 'data-testid': 'wiki-source-prefix' } }}
          onChange={(event) => {
            setPrefix(event.target.value);
            report('folder', bucket, event.target.value);
          }}
        />
      ) : null}
    </Stack>
  );
}
