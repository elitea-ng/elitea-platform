import type { ReactNode } from 'react';
import { useState } from 'react';

import AddIcon from '@mui/icons-material/Add';
import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';

import { useAgentSkills, useSkillPicker } from '../model/useAgentSkills';
import { SkillPickerMenu } from './SkillPickerMenu';

/**
 * The agent editor's SKILLS section.
 *
 * Production has it (`SKILLS` accordion, a `+ Skill` button, and the counter
 * "0/5 skills added."); this app could CREATE skills and never attach one, so
 * the whole Skills feature was write-only — a user could build a skill and had
 * no way to make an agent use it.
 *
 * The section is gated on a saved version. `entity_skill_mapping` is keyed by
 * `application_versions.id`, so there is no row to write before the agent has
 * been saved once. Rendering the picker over an unsaved draft would collect a
 * choice the attach call cannot honour, so the section says so instead.
 *
 * The skill's version is resolved HERE, not by the server: attach requires
 * `skill_version_id` and a skill whose attachment carries none is dropped at
 * run time (see `../api/agentSkillsApi.ts`). The list route returns the
 * skill's versions; the picker sends the first, which is the `base` version
 * every skill has.
 */
export interface AgentSkillsPanelProps {
  readonly projectId: string | undefined;
  /**
   * `application_versions.id` of the version open in the editor.
   *
   * A STRING, because that is what the version detail carries — the spec
   * describes it as "Numeric id serialized as string". The one conversion to a
   * number happens here, so no caller has to remember it.
   */
  readonly appVersionId: string | undefined;
  /** Read-only view (a public agent, or a viewer) hides the write controls. */
  readonly disabled?: boolean;
  readonly sx?: SxProps<Theme>;
}

export function AgentSkillsPanel({ projectId, appVersionId, disabled = false, sx }: AgentSkillsPanelProps): ReactNode {
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  const [query, setQuery] = useState('');
  // An unparseable id is treated as no id at all: the section then says the
  // agent must be saved first, rather than issuing a request for version NaN.
  const numericVersionId = appVersionId === undefined ? undefined : Number(appVersionId);
  const versionId = numericVersionId !== undefined && Number.isFinite(numericVersionId) ? numericVersionId : undefined;
  const skills = useAgentSkills(projectId, versionId);
  const picker = useSkillPicker(projectId, query, anchorEl !== null);

  const canAttach = !disabled && versionId !== undefined && !skills.isFull;
  const attachedIds = new Set(skills.attached.map((skill) => skill.id));

  return (
    <BasicAccordion
      data-testid="agent-skills-section"
      showMode="left"
      {...(sx === undefined ? {} : { slotSx: { root: sx } })}
      items={[
        {
          title: t('features.agentSkills.title', 'SKILLS'),
          content: (
            <Box sx={containerSx}>
              {versionId === undefined ? (
                <Typography variant="bodySmall">
                  {t('features.agentSkills.saveFirst', 'Save this agent once before you attach a skill to it.')}
                </Typography>
              ) : (
                <>
                  {!disabled && (
                    <BaseBtn
                      variant="secondary"
                      startIcon={<AddIcon />}
                      disabled={!canAttach || skills.attach.isPending}
                      data-testid="agent-add-skill-button"
                      onClick={(event) => setAnchorEl(event.currentTarget)}
                    >
                      {t('features.agentSkills.add', 'Skill')}
                    </BaseBtn>
                  )}
                  {skills.attached.map((skill) => (
                    <Box
                      key={skill.id}
                      sx={rowSx}
                      data-testid="agent-attached-skill"
                    >
                      <Box>
                        <Typography variant="bodyMedium">{skill.name}</Typography>
                        {skill.description !== undefined && skill.description !== '' && (
                          <Typography
                            variant="bodySmall"
                            color="text.secondary"
                          >
                            {skill.description}
                          </Typography>
                        )}
                      </Box>
                      {!disabled && (
                        <IconButton
                          aria-label={t('features.agentSkills.detach', 'Detach skill')}
                          data-testid={`agent-detach-skill-${String(skill.id)}`}
                          disabled={skills.detach.isPending}
                          onClick={() => skills.detach.mutate(skill.id)}
                          size="small"
                        >
                          <DeleteOutlinedIcon fontSize="small" />
                        </IconButton>
                      )}
                    </Box>
                  ))}
                  <Typography
                    variant="bodySmall"
                    data-testid="agent-skills-counter"
                  >
                    {t('features.agentSkills.counter', '{{attached}}/{{max}} skills added.', {
                      attached: skills.attached.length,
                      max: skills.max,
                    })}
                  </Typography>
                  {skills.isError && (
                    <Typography
                      role="alert"
                      variant="bodySmall"
                    >
                      {t('features.agentSkills.loadFailed', 'The attached skills could not be read.')}
                    </Typography>
                  )}
                </>
              )}
              <SkillPickerMenu
                anchorEl={anchorEl}
                query={query}
                onQueryChange={setQuery}
                options={picker.options}
                isLoading={picker.isLoading}
                attachedIds={attachedIds}
                onClose={() => setAnchorEl(null)}
                onPick={(skillId, pickedVersionId) => {
                  setAnchorEl(null);
                  skills.attach.mutate({ skillId, skillVersionId: pickedVersionId });
                }}
              />
            </Box>
          ),
        },
      ]}
    />
  );
}

const containerSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const rowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '0.5rem' };
