// Build-time materialization of micropip's frozen external wheel references.
const MAX_WHEEL_BYTES = 32 * 1024 * 1024;
const MAX_EXTERNAL_BYTES = 128 * 1024 * 1024;

export function wheelSource(entry) {
  const url = new URL(entry.file_name);
  const filename = url.pathname.split("/").at(-1);
  if (
    url.protocol !== "https:" || url.hostname !== "files.pythonhosted.org" ||
    url.port || url.username || url.password || url.search || url.hash ||
    !/^[A-Za-z0-9_.+-]+\.whl$/.test(filename) ||
    !/^[a-f0-9]{64}$/.test(entry.sha256)
  ) {
    throw new Error("Frozen wheel requires a PyPI source and SHA-256 digest");
  }
  return { url, filename };
}

async function verified(bytes, digest) {
  const actual = Array.from(
    new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)),
  )
    .map((part) => part.toString(16).padStart(2, "0")).join("");
  if (actual !== digest) {
    throw new Error("Frozen wheel digest does not match its lockfile");
  }
  return bytes;
}

export async function materializeWheels(lock, directory) {
  let total = 0;
  for (const entry of Object.values(lock.packages)) {
    const local = entry.file_name.startsWith(`${directory}/`);
    if (!local && !entry.file_name.startsWith("https:")) continue;
    const { url, filename } = local
      ? { url: null, filename: entry.file_name.slice(directory.length + 1) }
      : wheelSource(entry);
    if (
      !/^[A-Za-z0-9_.+-]+\.whl$/.test(filename) ||
      !/^[a-f0-9]{64}$/.test(entry.sha256)
    ) {
      throw new Error("Invalid frozen local wheel reference");
    }
    const path = `${directory}/${filename}`;
    let bytes;
    try {
      const stat = await Deno.lstat(path);
      if (!stat.isFile || stat.size > MAX_WHEEL_BYTES) {
        throw new Error("Invalid prepared wheel cache entry");
      }
      bytes = await verified(await Deno.readFile(path), entry.sha256);
    } catch (error) {
      if (!(error instanceof Deno.errors.NotFound) || local) throw error;
      const response = await fetch(url, {
        redirect: "error",
        signal: AbortSignal.timeout(60_000),
      });
      if (!response.ok || !response.body) {
        throw new Error("Frozen wheel download failed");
      }
      const chunks = [];
      let size = 0;
      for await (const chunk of response.body) {
        size += chunk.length;
        if (size > MAX_WHEEL_BYTES || total + size > MAX_EXTERNAL_BYTES) {
          throw new Error(
            "Frozen wheel download exceeds the package preparation limit",
          );
        }
        chunks.push(chunk);
      }
      bytes = new Uint8Array(size);
      let offset = 0;
      for (const chunk of chunks) {
        bytes.set(chunk, offset);
        offset += chunk.length;
      }
      await verified(bytes, entry.sha256);
      await Deno.writeFile(path, bytes, { createNew: true });
    }
    total += bytes.length;
    if (total > MAX_EXTERNAL_BYTES) {
      throw new Error("Prepared external wheels exceed 128 MiB");
    }
    // Pyodide 0.29 joins frozen absolute URLs to its CDN base. Materialize and
    // normalize them to cached basenames; preserve the native integrity digest.
    entry.file_name = filename;
  }
}
