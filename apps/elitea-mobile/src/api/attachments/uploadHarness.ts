/* Test-only helpers for the upload tests (in-memory files, multipart reading). */
import type { UploadDeps } from '@/api/attachments/upload';

/** An in-memory "disk": file bytes by uri, plus every temp file written and deleted. */
export function memoryDisk(files: Record<string, Uint8Array>) {
  const temps = new Map<string, Uint8Array>();
  const deleted: string[] = [];
  let n = 0;
  const deps: Pick<UploadDeps, 'part' | 'readChunk' | 'writeTemp'> = {
    // Node's FormData needs a Blob for a file part (React Native takes {uri,name,type}).
    part: (uri, name, type) => {
      const bytes = temps.get(uri) ?? files[uri];
      if (!bytes) throw new Error(`no such file ${uri}`);
      return new File([bytes as unknown as BlobPart], name, { type });
    },
    readChunk: async (uri, offset, length) => files[uri].slice(offset, offset + length),
    writeTemp: (bytes) => {
      const uri = `file:///cache/att-up/part-${++n}`;
      temps.set(uri, bytes);
      return {
        uri,
        delete: () => {
          temps.delete(uri);
          deleted.push(uri);
        },
      };
    },
  };
  return { deps, temps, deleted };
}

/** The text fields of a multipart body, and the file part's bytes as text. */
export function readMultipart(body: string): { fields: Record<string, string>; file: string | null; fileName: string | null } {
  const fields: Record<string, string> = {};
  let file: string | null = null;
  let fileName: string | null = null;
  const boundary = body.slice(0, body.indexOf('\r\n'));
  for (const part of body.split(boundary)) {
    const match = /name="([^"]+)"(?:; filename="([^"]*)")?[\s\S]*?\r\n\r\n([\s\S]*)\r\n$/.exec(part);
    if (!match) continue;
    if (match[2] !== undefined) {
      file = match[3];
      fileName = match[2];
    } else fields[match[1]] = match[3];
  }
  return { fields, file, fileName };
}
