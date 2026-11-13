import { useCallback, useMemo, useRef, useState } from 'react';

import { uploadArtifactObject } from '@/shared/api/artifacts';
import { getConfig } from '@/shared/config';

import { useChatConfig } from '../api/chatConfigApi';
import { describeUploadFailure } from '../lib/uploadFailure';
import { computeSecurePath, validateFileName, validateFolderPath } from '../lib/pathValidation';
import type { ArtifactUploadPlan } from './types';
import type { Artifact } from '@/entities/artifact';

const DEFAULT_MAX_FILE_SIZE = 150 * 1024 * 1024;

function readUploadLimit(value: unknown): number {
  if (typeof value !== 'object' || value === null) return DEFAULT_MAX_FILE_SIZE;
  const record = value as Record<string, unknown>;
  const limits = typeof record.limits === 'object' && record.limits !== null
    ? record.limits as Record<string, unknown>
    : undefined;
  const bytes = limits?.DEFAULT_MAX_FILE_SIZE;
  if (typeof bytes === 'number' && Number.isFinite(bytes) && bytes > 0) return bytes;
  const megabytes = record.chat_max_file_upload_size_mb;
  return typeof megabytes === 'number' && Number.isFinite(megabytes) && megabytes > 0
    ? megabytes * 1024 * 1024
    : DEFAULT_MAX_FILE_SIZE;
}

export function buildArtifactUploadPlan(
  files: readonly File[],
  contents: readonly Artifact[],
  folderPath: string,
  currentPrefix: string,
  maxFileSize: number,
): ArtifactUploadPlan {
  const pathError = validateFolderPath(folderPath, currentPrefix);
  if (pathError !== '') throw new Error(pathError);
  const securePath = computeSecurePath(folderPath, currentPrefix);
  const targetPrefix = securePath === '' ? '' : `${securePath}/`;
  const existingKeys = new Set(contents.map((artifact) => artifact.key));
  const accepted: File[] = [];
  const rejected: { file: File; reason: string }[] = [];
  const duplicates: string[] = [];
  for (const file of files) {
    const nameError = validateFileName(file.name);
    if (nameError !== '') rejected.push({ file, reason: nameError });
    else if (file.size > maxFileSize) rejected.push({ file, reason: 'File exceeds the upload size limit.' });
    else {
      accepted.push(file);
      if (existingKeys.has(`${targetPrefix}${file.name}`)) duplicates.push(file.name);
    }
  }
  return { accepted, rejected, duplicates, targetPrefix };
}

export function keepBothFileNames(
  files: readonly File[],
  contents: readonly Artifact[],
  targetPrefix: string,
): File[] {
  const reserved = new Set(
    contents
      .filter((artifact) => artifact.key.startsWith(targetPrefix))
      .map((artifact) => artifact.key.slice(targetPrefix.length))
      .filter((name) => !name.includes('/')),
  );
  return files.map((file) => {
    if (!reserved.has(file.name)) {
      reserved.add(file.name);
      return file;
    }
    const dot = file.name.lastIndexOf('.');
    const base = dot > 0 ? file.name.slice(0, dot) : file.name;
    const extension = dot > 0 ? file.name.slice(dot) : '';
    let index = 1;
    let name = `${base} - Copy${extension}`;
    while (reserved.has(name)) {
      index += 1;
      name = `${base} - Copy (${index})${extension}`;
    }
    reserved.add(name);
    return new File([file], name, { type: file.type, lastModified: file.lastModified });
  });
}

interface UseArtifactUploadOptions {
  readonly projectId?: string;
  readonly bucket?: string;
  readonly contents: readonly Artifact[];
  readonly currentPrefix: string;
  readonly onUploaded: () => unknown;
}

export function useArtifactUpload(options: UseArtifactUploadOptions) {
  const inputRef = useRef<HTMLInputElement>(null);
  const [pendingFiles, setPendingFiles] = useState<readonly File[]>([]);
  const [pendingPlan, setPendingPlan] = useState<ArtifactUploadPlan>();
  const [pathDialogOpen, setPathDialogOpen] = useState(false);
  const [duplicateDialogOpen, setDuplicateDialogOpen] = useState(false);
  const [isUploading, setIsUploading] = useState(false);
  const [error, setError] = useState<string>();
  const chatConfig = useChatConfig(options.projectId);
  const maxFileSize = useMemo(() => readUploadLimit(chatConfig.data), [chatConfig.data]);

  const chooseFiles = useCallback(() => inputRef.current?.click(), []);
  const stageFiles = useCallback((files: readonly File[]) => {
    if (files.length === 0) return;
    setPendingFiles(files);
    setError(undefined);
    setPathDialogOpen(true);
  }, []);

  const upload = useCallback(async (files: readonly File[], targetPrefix: string) => {
    const { projectId, bucket } = options;
    if (projectId === undefined || bucket === undefined) return;
    const config = getConfig();
    if (config.status !== 'ok') {
      setError('Runtime configuration is unavailable.');
      return;
    }
    setIsUploading(true);
    setError(undefined);
    try {
      // Every file uploads concurrently, best-effort — one failing file must not
      // stop the others (baseline: slices/upload.js:89-159's Promise.allSettled).
      const results = await Promise.allSettled(
        files.map((file) =>
          uploadArtifactObject({
            baseUrl: config.config.vite_server_url,
            projectId,
            bucket,
            fileKey: `${targetPrefix}${file.name}`,
            file,
          }).then((result) => {
            if (!result.ok) throw new Error(describeUploadFailure(result.error));
          }),
        ),
      );
      const failedNames: string[] = [];
      const reasons = new Set<string>();
      results.forEach((result, index) => {
        if (result.status !== 'rejected') return;
        failedNames.push(files[index]?.name ?? '');
        if (result.reason instanceof Error) reasons.add(result.reason.message);
      });
      if (failedNames.length > 0) {
        setError(`Failed to upload: ${failedNames.join(', ')}. ${[...reasons].join(' ')}`.trim());
      }
      await options.onUploaded();
      setPendingFiles([]);
      setPendingPlan(undefined);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Upload failed.');
    } finally {
      setIsUploading(false);
    }
  }, [options]);

  const confirmPath = useCallback((path: string) => {
    try {
      const plan = buildArtifactUploadPlan(pendingFiles, options.contents, path, options.currentPrefix, maxFileSize);
      setPathDialogOpen(false);
      if (plan.rejected.length > 0) {
        setError(plan.rejected.map((issue) => `${issue.file.name}: ${issue.reason}`).join(' '));
      }
      if (plan.accepted.length === 0) return;
      if (plan.duplicates.length > 0) {
        setPendingPlan(plan);
        setDuplicateDialogOpen(true);
      } else {
        void upload(plan.accepted, plan.targetPrefix);
      }
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Invalid upload path.');
    }
  }, [maxFileSize, options.contents, options.currentPrefix, pendingFiles, upload]);

  const cancelDuplicates = useCallback(() => {
    setDuplicateDialogOpen(false);
    setPendingPlan(undefined);
  }, []);
  const replaceDuplicates = useCallback(() => {
    if (pendingPlan === undefined) return;
    setDuplicateDialogOpen(false);
    void upload(pendingPlan.accepted, pendingPlan.targetPrefix);
  }, [pendingPlan, upload]);
  const skipDuplicates = useCallback(() => {
    if (pendingPlan === undefined) return;
    const duplicates = new Set(pendingPlan.duplicates);
    setDuplicateDialogOpen(false);
    void upload(pendingPlan.accepted.filter((file) => !duplicates.has(file.name)), pendingPlan.targetPrefix);
  }, [pendingPlan, upload]);
  const keepBoth = useCallback(() => {
    if (pendingPlan === undefined) return;
    setDuplicateDialogOpen(false);
    void upload(
      keepBothFileNames(pendingPlan.accepted, options.contents, pendingPlan.targetPrefix),
      pendingPlan.targetPrefix,
    );
  }, [options.contents, pendingPlan, upload]);

  return {
    inputRef,
    chooseFiles,
    stageFiles,
    pathDialogOpen,
    closePathDialog: () => setPathDialogOpen(false),
    confirmPath,
    duplicateDialogOpen,
    duplicateFilenames: pendingPlan?.duplicates ?? [],
    cancelDuplicates,
    replaceDuplicates,
    skipDuplicates,
    keepBoth,
    isUploading,
    error,
    maxFileSize,
  };
}
