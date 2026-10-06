import { createContext, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { codeDebugToolMeta, parseCodeDebugProof, type CodeDebugProof } from '@/shared/lib/codeDebugArtifact';
import { triggerBlobDownload } from '@/shared/lib/download';
import { readCodeDebugArtifact, type CodeDebugReadResult } from '@/shared/lib/readCodeDebugArtifact';

/** The selected product project comes from the caller, never the artifact. */
export const CodeDebugProjectContext = createContext<string | undefined>(undefined);

interface Props {
  readonly value: unknown;
  readonly projectId: string | undefined;
  readonly scopeKey: string;
}

function readGuidance(reason: Extract<CodeDebugReadResult, { ok: false }>['reason']): string {
  switch (reason) {
    case 'authorization': return t('codeDebug.readDenied', 'Access is denied. Check your sign-in and current artifact permissions.');
    case 'changed': return t('codeDebug.readChanged', 'The artifact bytes do not match this reference. No download was created.');
    case 'verification': return t('codeDebug.readVerification', 'This browser cannot verify the artifact. No download was created.');
    case 'aborted':
    case 'unavailable': return t('codeDebug.readUnavailable', 'The artifact could not be read. Check access and availability, then retry.');
  }
}

function CodeDebugArtifactView({ proof, projectId }: { readonly proof: CodeDebugProof; readonly projectId: string }): ReactNode {
  const request = useRef<AbortController | undefined>(undefined);
  const [busy, setBusy] = useState(false);
  const [guidance, setGuidance] = useState<string>();
  useLayoutEffect(() => () => { request.current?.abort(); }, []);

  const download = async (): Promise<void> => {
    if (request.current || !proof.artifact) return;
    const controller = new AbortController();
    request.current = controller;
    setBusy(true);
    setGuidance(undefined);
    try {
      const result = await readCodeDebugArtifact(proof.artifact, projectId, controller.signal);
      if (controller.signal.aborted) return;
      if (result.ok) triggerBlobDownload(result.blob, `code-debug-attempt-${proof.attempt}.json`);
      else setGuidance(readGuidance(result.reason));
    } catch {
      if (!controller.signal.aborted) setGuidance(readGuidance('unavailable'));
    } finally {
      if (request.current === controller) request.current = undefined;
      if (!controller.signal.aborted) setBusy(false);
    }
  };

  return <Box sx={{ padding: 2, gridColumn: '1 / -1', overflowWrap: 'anywhere' }} data-testid="code-debug-artifact">
    <Typography variant="bodySmall" component="p">{t('codeDebug.attempt', 'Code debug · {{node}} · attempt {{attempt}}', { node: proof.node_id, attempt: proof.attempt })}</Typography>
    {proof.status === 'committed' && proof.artifact ? <>
      <Typography variant="bodySmall" component="p">{t('codeDebug.size', '{{bytes}} bytes · SHA-256 {{digest}}', { bytes: proof.artifact.byte_length, digest: proof.artifact.sha256 })}</Typography>
      <Typography variant="bodySmall" component="p">{t('codeDebug.readAcl', 'Download requires current artifact access. The bytes are verified before download.')}</Typography>
      <Typography variant="bodySmall" component="p">{t('codeDebug.contents', 'The snapshot contains executable Code source and selected input state for this attempt. Review the source before running it.')}</Typography>
      <Button disabled={busy} onClick={() => { void download(); }}>{busy ? t('codeDebug.verifying', 'Verifying...') : t('codeDebug.download', 'Download verified snapshot')}</Button>
    </> : <Typography component="output">{proof.status === 'denied'
      ? t('codeDebug.exportDenied', 'The Code debug export was denied. Check debug export permissions. Code execution continues.')
      : t('codeDebug.exportUnavailable', 'The Code debug artifact is unavailable. Code execution continues.')}</Typography>}
    {guidance && <Typography role="alert">{guidance}</Typography>}
  </Box>;
}

/** A complete visit identity remounts the read scope. Returning to an old visit cannot revive a read. */
export function CodeDebugArtifact({ value, projectId, scopeKey }: Props): ReactNode {
  if (value === undefined) return null;
  const proof = parseCodeDebugProof(value);
  if (!proof) return <Typography component="output">{t('codeDebug.invalidReference', 'The Code debug reference is invalid. No artifact can be downloaded.')}</Typography>;
  if (proof.artifact && (!projectId || projectId !== String(proof.artifact.project_id))) {
    return <Typography component="output">{t('codeDebug.projectUnavailable', 'The artifact project does not match the selected project. No artifact can be downloaded.')}</Typography>;
  }
  const identity = JSON.stringify([projectId, scopeKey, proof]);
  return <CodeDebugArtifactView key={identity} proof={proof} projectId={projectId ?? ''} />;
}

/** Consume only matching public metadata projected by Main. */
export function CodeDebugTraceArtifact({ attrs, projectId, scopeKey }: { readonly attrs: unknown; readonly projectId: string | undefined; readonly scopeKey: string }): ReactNode {
  return <CodeDebugArtifact value={codeDebugToolMeta(attrs)['code_debug_v1']} projectId={projectId} scopeKey={scopeKey} />;
}
