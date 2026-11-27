/**
 * One `.agent.md` file (the file Export writes with `?format=md`) read into
 * the import entry (#1996), with the `elitea.agent/v1` keys of issue 3137:
 * the format marker, `reasoning_effort`, the rich meta blocks, the avatar,
 * and the application-level evaluation datasets and memory seeds. The
 * `blueprint:` block is gallery metadata and is never read here.
 */
import { load as loadYaml } from 'js-yaml';

import { t } from '@/shared/i18n';

import { AGENT_FILE_SCHEMA, checkFileSchema } from './fileSchema';
import { ImportDocumentError } from './importError';
import {
  asRecord,
  buildEntry,
  collectSkills,
  collectToolkits,
  frontAgentType,
  frontAvatar,
  frontName,
  frontVersion,
  newKey,
  pipelineInstructions,
} from './importMarkdown';
import type { ParsedMarkdown } from './importTypes';

const FRONTMATTER = /^---\s*\r?\n([\s\S]*?)\r?\n---[ \t]*(?:\r?\n([\s\S]*))?$/;

/** Reads of a single `.md`, and of each zip entry, stop here (bytes). */
export const MAX_FILE_BYTES = 1024 * 1024;

export function tooLarge(name: string, limit: number): ImportDocumentError {
  return new ImportDocumentError(
    t('features.agentLifecycle.import.tooLarge', '{{name}} is larger than {{limit}} MiB, which is the import limit.', {
      name,
      limit: limit / (1024 * 1024),
    }),
  );
}

function readFront(text: string): { readonly front: Record<string, unknown>; readonly body: string } {
  const match = FRONTMATTER.exec(text.replace(/^﻿/, ''));
  if (match === null) {
    throw new ImportDocumentError(
      t(
        'features.agentLifecycle.import.noFrontmatter',
        'This file must start with YAML frontmatter (name, description, model, instructions).',
      ),
    );
  }
  let front: Record<string, unknown>;
  try {
    front = asRecord(loadYaml(match[1] ?? ''));
  } catch {
    throw new ImportDocumentError(
      t('features.agentLifecycle.import.badFrontmatter', 'The YAML frontmatter of this file is not valid.'),
    );
  }
  checkFileSchema(front, AGENT_FILE_SCHEMA);
  return { front, body: match[2] ?? '' };
}

export function parseMarkdownFull(text: string): ParsedMarkdown {
  const { front, body } = readFront(text);
  const name = frontName(front);
  if (name === '') {
    throw new ImportDocumentError(t('features.agentLifecycle.import.noName', 'The frontmatter of this file has no name.'));
  }
  const agentType = frontAgentType(front);
  const instructions = agentType === 'pipeline' ? pipelineInstructions(front) : body.trim();
  const version = frontVersion(front);
  // A fresh key per entry. A slug of the name collided for non-Latin names and
  // for names differing only in punctuation, and the server keys the import on
  // it, so two agents of one zip would have merged.
  const uuid = newKey('md');
  const { toolkits, tools, skipped: toolkitSkipped } = collectToolkits(front, name);
  const { skills, skillRefs, skipped: skillSkipped } = collectSkills(front, name);
  const skipped = [...toolkitSkipped, ...skillSkipped];
  if (front['pipeline_settings'] !== undefined) skipped.push(`${name}: pipeline_settings`);
  const avatar = frontAvatar(front);
  if (avatar.unreadable === true) {
    skipped.push(t('features.agentLifecycle.import.avatarUnreadable', '{{name}}: avatar (not a default avatar or a map of files)', { name }));
  }
  const entry = buildEntry({ front, name, agentType, instructions, version, uuid, tools, skillRefs, avatarPack: avatar.pack });
  return { entry, toolkits, skills, skipped, avatarPaths: avatar.paths };
}
