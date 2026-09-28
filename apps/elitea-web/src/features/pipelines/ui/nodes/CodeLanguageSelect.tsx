import { useContext, type ReactNode } from 'react';

import { SingleSelect } from '@/shared/ui/SingleSelect';
import { t } from '@/shared/i18n';

import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import { updateYamlNode } from '../../lib/flow-editor/helpers/flowEditor.helpers';

const LANGUAGES = [
  { value: 'python', label: 'Python' },
  { value: 'javascript', label: 'JavaScript' },
  { value: 'rust', label: 'Rust' },
];

export function CodeLanguageSelect({ id, disabled }: { readonly id: string; readonly disabled: boolean }): ReactNode {
  const context = useContext(FlowEditorContext);
  const node = context?.yamlJsonObject.nodes?.find(candidate => candidate.id === id);
  // Omission preserves the existing Python YAML contract without dirtying the document.
  const language = node?.language ?? 'python';

  return (
    <div className="nopan nodrag">
      <SingleSelect
        label={t('pipelines.flowEditor.codeNode.language', 'Language')}
        value={typeof language === 'string' ? language : ''}
        options={LANGUAGES}
        disabled={disabled}
        onChange={value => {
          if (context && LANGUAGES.some(option => option.value === value)) {
            updateYamlNode(id, 'language', value, context.yamlJsonObject, context.setYamlJsonObject);
          }
        }}
      />
    </div>
  );
}
