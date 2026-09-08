/**
 * ConfigurationsPanel — displays the header with AddModelButton and all
 * configuration sections (LLM, Embedding, Vector Storage, Image, ASR, TTS,
 * AI Credentials).
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/ai-configuration/Configuration/ConfigurationsPanel.jsx`.
 */
import { memo, useCallback, useEffect, useMemo, useRef } from 'react';
import { useTheme, type Theme } from '@mui/material/styles';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';

import { useDefaultModelSaving } from '../../lib/ai-configuration/useDefaultModelSaving';
import { useModelOptions } from '../../lib/ai-configuration/useModelOptions';
import {
  StoredConnectionHealthProvider,
  collectConfigurationIds,
  useStoredConnectionHealth,
} from '../../lib/ai-configuration/useStoredConnectionHealth';

import AddModelButton from './AddModelButton';
import {
  computeProjectGating,
  defaultValueOf,
  sectionHoldsRevealedRow,
  tierDefaultValueOf,
} from './configurationsPanel.helpers';
import ConfigurationSection, {
  type AdditionalDefaultSetting,
} from './ConfigurationSection';

/* ── types ──────────────────────────────────────────────────────────────── */

interface ConfigurationsPanelProps {
  /** Map of section name → configuration items. */
  configurationsBySection: Record<string, Record<string, unknown>[]>;
  /** Currently-selected project id — threaded down to `ConfigurationSection`
   * for edit-permission gating/click-to-edit, and used to fetch/persist
   * each section's real default model. */
  projectId: string;
  isLoading: boolean;
  /** Just-saved/edited configuration id (`?reveal=`) — opens the section holding it. */
  revealConfigurationId?: string;
}

/* ── component ──────────────────────────────────────────────────────────── */

export default memo(function ConfigurationsPanel({
  configurationsBySection,
  projectId,
  isLoading,
  revealConfigurationId,
}: ConfigurationsPanelProps) {
  const theme = useTheme();
  const styles = getStyles(theme);
  const panelRef = useRef<HTMLDivElement | null>(null);

  /* Extract sections safely (TS doesn't know the key types) */
  const llmConfigs = configurationsBySection['llm'] ?? [];
  const embeddingConfigs = configurationsBySection['embedding'] ?? [];
  const vectorStorageConfigs = configurationsBySection['vectorstorage'] ?? [];
  const imageConfigs = configurationsBySection['image_generation'] ?? [];
  const asrConfigs = configurationsBySection['asr'] ?? [];
  const ttsConfigs = configurationsBySection['tts'] ?? [];
  const aiCredentialsConfigs = configurationsBySection['ai_credentials'] ?? [];

  const { includeShared, canCreateConfiguration } = useMemo(
    () => computeProjectGating(projectId),
    [projectId],
  );

  /*
   * THE OPTIONS AND THE DEFAULTS COME FROM ONE PLACE (#80). Both are the
   * MODEL CATALOGUE — `GET /configurations/models/{projectId}` — which is what
   * the baseline's `useModelOptions` reads and what the POST that saves a
   * default expects.
   *
   * They used to disagree. The options were built from the CONFIGURATION rows
   * and labelled with `elitea_title`, while every `defaultValueOf` below is
   * `${default_model_name}<<>>${default_model_project_id}` — a model name. No
   * option ever equalled the select's value, so the Default select could not
   * show the model that was the default, and a section with no configuration
   * rows (production project 1 has none for embedding or vector storage) had
   * no options at all. See `useModelOptions` for the measurements.
   */
  const models = useModelOptions({ projectId, includeShared });
  const llmDefaults = models.sectionData.llm;
  const embeddingDefaults = models.sectionData.embedding;
  const vectorStorageDefaults = models.sectionData.vectorstorage;
  const imageDefaults = models.sectionData.image_generation;
  const asrDefaults = models.sectionData.asr;
  const ttsDefaults = models.sectionData.tts;

  const { saveErrors, handleDefaultChange } = useDefaultModelSaving(projectId);

  /* Per-card connection health. NOTHING fires on mount — a project can hold
     dozens of configurations and each check is a real provider round trip, so
     the user asks for it with the button in the header below and presses it
     again to refresh. */
  const configurationIds = collectConfigurationIds(configurationsBySection);
  const connectionHealth = useStoredConnectionHealth(projectId, configurationIds);
  const { health, revalidate, revalidatingId, checkAll, isChecking, checkError } = connectionHealth;
  const healthView = useMemo(
    () => ({ health, revalidate, revalidatingId }),
    [health, revalidate, revalidatingId],
  );

  /* Default-setting labels — text plus the info icon that carries the
     tooltip. The icon is what the production page shows next to every one of
     these labels; it was dropped in this port, taking the only explanation of
     what "High-tier" means with it. */
  const renderInfoLabel = useCallback(
    (labelText: string, tooltipText: string) => (
      <InfoLabelWithTooltip
        label={labelText}
        tooltip={tooltipText}
        variant="bodyMedium"
        iconSize={12}
      />
    ),
    [],
  );

  /* LLM section needs extra low-tier / high-tier selects */
  const llmAdditionalSettings: AdditionalDefaultSetting[] = useMemo(
    () => [
      {
        key: 'high-tier-model',
        label: renderInfoLabel(
          t('ai-configuration.section.highTier', 'High-tier'),
          t(
            'ai-configuration.section.highTierTooltip',
            'Model used for complex tasks that require stronger reasoning or higher-quality responses.',
          ),
        ),
        value: tierDefaultValueOf(llmDefaults.high_tier_default_model_name, llmDefaults.high_tier_default_model_project_id),
        options: models.highTierModelOptions,
        onChange: handleDefaultChange('llm_high_tier'),
        ...(saveErrors['llm_high_tier'] !== undefined ? { error: saveErrors['llm_high_tier'] } : {}),
      },
      {
        key: 'low-tier-model',
        label: renderInfoLabel(
          t('ai-configuration.section.lowTier', 'Low-tier'),
          t(
            'ai-configuration.section.lowTierTooltip',
            'Model used for simpler tasks where faster responses and lower cost are preferred.',
          ),
        ),
        value: tierDefaultValueOf(llmDefaults.low_tier_default_model_name, llmDefaults.low_tier_default_model_project_id),
        options: models.lowTierModelOptions,
        onChange: handleDefaultChange('llm_low_tier'),
        ...(saveErrors['llm_low_tier'] !== undefined ? { error: saveErrors['llm_low_tier'] } : {}),
      },
    ],
    [renderInfoLabel, models.highTierModelOptions, models.lowTierModelOptions, handleDefaultChange, llmDefaults, saveErrors],
  );

  // Scrolls the revealed card into view (its section can open below the fold).
  useEffect(() => {
    if (revealConfigurationId === undefined || isLoading) return;
    const card = panelRef.current?.querySelector(`[data-configuration-id="${revealConfigurationId}"]`);
    if (card instanceof HTMLElement && typeof card.scrollIntoView === 'function') {
      card.scrollIntoView({ block: 'center' });
    }
  }, [revealConfigurationId, isLoading, configurationsBySection]);

  return (
    <Box sx={styles.panel} ref={panelRef}>
      {/*
        A TOOLBAR, not a second title bar. This row used to carry a
        `headingMedium` "Configurations" heading and a bottom rule of its own,
        directly under the page's own header — two titles, two rules, 60px of
        chrome for one screen. Production shows neither: its equivalent of the
        "+" lives in the app rail. The controls stay (they do real work that
        has no other home here); the heading and the rule are gone, and the
        row is right-aligned so it reads as the accordions' toolbar.
      */}
      <Box sx={styles.header}>
        <Box sx={styles.headerContent}>
          <Box sx={styles.headerActions}>
            {checkError !== '' && (
              <Typography variant="bodySmall" sx={styles.checkError} role="alert">
                {checkError}
              </Typography>
            )}
            <BaseBtn
              variant="secondary"
              size="small"
              data-testid="check-connections"
              disabled={isChecking || configurationIds.length === 0}
              onClick={checkAll}
            >
              {isChecking
                ? t('ai-configuration.health.checkingAll', 'Checking connections...')
                : t('ai-configuration.health.checkAll', 'Check connections')}
            </BaseBtn>
            {canCreateConfiguration && <AddModelButton />}
          </Box>
        </Box>
      </Box>

      {/* Every card's health dot and its Re-validate action come from here.
          Context, not a prop: `ConfigurationSection` already destructures 12
          props — the §3.5 budget — and never reads this value, it only
          forwards it to the cards. See `StoredConnectionHealthView`. */}
      <StoredConnectionHealthProvider value={healthView}>
        {/* LLM Models */}
        <ConfigurationSection
          title={t('ai-configuration.section.llms', 'LLMs')}
          display={{ groupByProvider: true, defaultExpanded: true, testId: 'ai-providers-section-llms' }}
          configurations={llmConfigs}
          projectId={projectId}
          isLoading={isLoading}
          hasDefaultSetting
          defaultSettingLabel={renderInfoLabel(
            t('ai-configuration.section.default', 'Default'),
            t(
              'ai-configuration.section.defaultTooltip',
              'Default model used for most AI activities. The system may switch to the Low-tier or High-tier model when needed.',
            ),
          )}
          defaultSettingValue={defaultValueOf(llmDefaults)}
          defaultSettingOptions={models.modelOptions}
          onChangeDefaultSetting={handleDefaultChange('llm')}
          defaultSettingError={saveErrors['llm']}
          additionalDefaultSettings={llmAdditionalSettings}
        />

        {/* Embedding Models */}
        <ConfigurationSection
          title={t('ai-configuration.section.embeddingModels', 'Embedding Models')}
          display={{ testId: 'ai-providers-section-embedding-models', defaultExpanded: sectionHoldsRevealedRow(embeddingConfigs, revealConfigurationId) }}
          configurations={embeddingConfigs}
          projectId={projectId}
          isLoading={isLoading}
          hasDefaultSetting
          defaultSettingLabel={renderInfoLabel(
            t('ai-configuration.section.default', 'Default'),
            t(
              'ai-configuration.section.embeddingTooltip',
              'Default embedding model used to convert content into vectors for indexing, semantic search, and retrieval.',
            ),
          )}
          defaultSettingValue={defaultValueOf(embeddingDefaults)}
          defaultSettingOptions={models.embeddingModelOptions}
          onChangeDefaultSetting={handleDefaultChange('embedding')}
          defaultSettingError={saveErrors['embedding']}
        />

        {/* Vector Storage */}
        <ConfigurationSection
          title={t('ai-configuration.section.vectorStorage', 'Vector Storage')}
          display={{ testId: 'ai-providers-section-vector-storage', defaultExpanded: sectionHoldsRevealedRow(vectorStorageConfigs, revealConfigurationId) }}
          configurations={vectorStorageConfigs}
          projectId={projectId}
          isLoading={isLoading}
          hasDefaultSetting
          defaultSettingLabel={renderInfoLabel(
            t('ai-configuration.section.default', 'Default'),
            t(
              'ai-configuration.section.vectorStorageTooltip',
              'Default vector storage used to store embeddings for indexing, search, and retrieval.',
            ),
          )}
          defaultSettingValue={defaultValueOf(vectorStorageDefaults)}
          defaultSettingOptions={models.vectorStorageOptions}
          onChangeDefaultSetting={handleDefaultChange('vectorstorage')}
          defaultSettingError={saveErrors['vectorstorage']}
        />

        {/* Image Generation */}
        <ConfigurationSection
          title={t('ai-configuration.section.imageGeneration', 'Image Generation')}
          display={{ testId: 'ai-providers-section-image-generation', defaultExpanded: sectionHoldsRevealedRow(imageConfigs, revealConfigurationId) }}
          configurations={imageConfigs}
          projectId={projectId}
          isLoading={isLoading}
          hasDefaultSetting
          defaultSettingLabel={renderInfoLabel(
            t('ai-configuration.section.default', 'Default'),
            t(
              'ai-configuration.section.imageTooltip',
              'Default image generation model used when creating images in Elitea.',
            ),
          )}
          defaultSettingValue={defaultValueOf(imageDefaults)}
          defaultSettingOptions={models.imageGenerationOptions}
          onChangeDefaultSetting={handleDefaultChange('image_generation')}
          defaultSettingError={saveErrors['image_generation']}
        />

        {/* Speech Recognition (ASR) */}
        <ConfigurationSection
          title={t('ai-configuration.section.asr', 'Speech Recognition (ASR)')}
          display={{ testId: 'ai-providers-section-asr', defaultExpanded: sectionHoldsRevealedRow(asrConfigs, revealConfigurationId) }}
          configurations={asrConfigs}
          projectId={projectId}
          isLoading={isLoading}
          hasDefaultSetting
          defaultSettingLabel={renderInfoLabel(
            t('ai-configuration.section.default', 'Default'),
            t(
              'ai-configuration.section.asrTooltip',
              'Default speech recognition model used to convert audio or speech into text.',
            ),
          )}
          defaultSettingValue={defaultValueOf(asrDefaults)}
          defaultSettingOptions={models.asrOptions}
          onChangeDefaultSetting={handleDefaultChange('asr')}
          defaultSettingError={saveErrors['asr']}
        />

        {/* Text to Speech (TTS) */}
        <ConfigurationSection
          title={t('ai-configuration.section.tts', 'Text to Speech (TTS)')}
          display={{ testId: 'ai-providers-section-tts', defaultExpanded: sectionHoldsRevealedRow(ttsConfigs, revealConfigurationId) }}
          configurations={ttsConfigs}
          projectId={projectId}
          isLoading={isLoading}
          hasDefaultSetting
          defaultSettingLabel={renderInfoLabel(
            t('ai-configuration.section.default', 'Default'),
            t(
              'ai-configuration.section.ttsTooltip',
              'Default text-to-speech model used to convert text into spoken audio.',
            ),
          )}
          defaultSettingValue={defaultValueOf(ttsDefaults)}
          defaultSettingOptions={models.ttsOptions}
          onChangeDefaultSetting={handleDefaultChange('tts')}
          defaultSettingError={saveErrors['tts']}
        />

        {/* AI Credentials */}
        <ConfigurationSection
          title={t('ai-configuration.section.aiCredentials', 'AI Credentials')}
          display={{ testId: 'ai-providers-section-ai-credentials', defaultExpanded: sectionHoldsRevealedRow(aiCredentialsConfigs, revealConfigurationId) }}
          configurations={aiCredentialsConfigs}
          projectId={projectId}
          isLoading={isLoading}
        />
      </StoredConnectionHealthProvider>
    </Box>
  );
});

function getStyles(theme: ReturnType<typeof useTheme>) {
  const t = theme as Theme;
  return {
    panel: {
      flex: 1,
      minHeight: 0,
      overflow: 'auto',
      height: '100%',
    },
    header: {
      display: 'flex',
      justifyContent: 'flex-end',
      alignItems: 'center',
      position: 'sticky',
      top: 0,
      backgroundColor: t.vars.palette.background.settingsPage,
      zIndex: 1,
      width: '100%',
    },
    headerContent: {
      display: 'flex',
      justifyContent: 'flex-end',
      alignItems: 'center',
      width: '100%',
      padding: '0.5rem 1.5rem 0',
    },
    headerActions: {
      display: 'flex',
      alignItems: 'center',
      gap: '0.5rem',
      flexShrink: 0,
    },
    checkError: {
      color: t.vars.palette.status.rejectedText,
    },
    inlineDefaultLabel: {
      display: 'flex',
      alignItems: 'center',
      gap: '0.25rem',
    },
    inlineDefaultLabelText: {
      fontWeight: 500,
      whiteSpace: 'nowrap',
    },
  };
}
