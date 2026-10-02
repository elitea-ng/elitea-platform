import { useFormikContext } from 'formik';

import { t } from '@/shared/i18n';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import type { SettingsProfileFormValues } from '../ai-personality/settingsProfileForm';

export interface SummaryModelOption {
  readonly name: string;
  readonly project_id: string | number;
  readonly display_name?: string;
}

export function SummaryModelSelect({ models, disabled = false }: {
  readonly models: readonly SummaryModelOption[];
  readonly disabled?: boolean;
}) {
  const { values, setFieldValue } = useFormikContext<Pick<SettingsProfileFormValues, 'summary_llm_settings'>>();
  const settings = values.summary_llm_settings;
  const value = settings.model_name ? JSON.stringify([settings.model_name, settings.model_project_id]) : '';
  const options = [
    { value: '', label: t('settings.memory.summaryModel.inherit', 'Use the chat model') },
    ...models.map((model) => ({
      value: JSON.stringify([model.name, String(model.project_id)]),
      label: `${model.display_name ?? model.name} (${String(model.project_id)})`,
    })),
  ];
  if (value && !options.some((option) => option.value === value)) {
    options.push({ value, label: t('settings.memory.summaryModel.unavailable', '{{model}} — unavailable in this project', { model: settings.model_name }) });
  }
  return <SingleSelect
    label={t('settings.memory.summaryModel.label', 'Summarization model')}
    value={value}
    options={options}
    disabled={disabled}
    onChange={(next) => {
      const model = models.find((item) => JSON.stringify([item.name, String(item.project_id)]) === next);
      if (next !== '' && !model) return;
      void setFieldValue('summary_llm_settings', {
        ...settings,
        model_name: model?.name ?? '',
        model_project_id: model ? String(model.project_id) : null,
      });
    }}
  />;
}
