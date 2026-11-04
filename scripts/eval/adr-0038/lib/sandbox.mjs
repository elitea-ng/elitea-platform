// Throwaway workspaces and the four worker tools.
// Commands run with a time limit, an output cap and no network.
// On macOS the harness uses sandbox-exec: no network, and writes only in the
// run directory and the shared Go build cache.
import { spawn, spawnSync } from 'node:child_process';
import {
  mkdtempSync, mkdirSync, cpSync, rmSync, readFileSync, writeFileSync,
  readdirSync, statSync, lstatSync, realpathSync, existsSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve, relative, dirname, sep } from 'node:path';

// The Go build cache, as `go env GOCACHE` reports it (it honours a GOCACHE
// in the environment). Commands may write there. Without `go` on PATH, a
// directory in the system temp dir.
let goCacheDir;
export function goCache() {
  if (goCacheDir !== undefined) return goCacheDir;
  const r = spawnSync('go', ['env', 'GOCACHE'], { encoding: 'utf8' });
  const dir = r.status === 0 ? r.stdout.trim() : '';
  goCacheDir = dir && dir !== 'off' ? dir : join(tmpdir(), 'adr0038-gocache');
  return goCacheDir;
}

let sandboxKind;
export function sandboxKindAvailable() {
  if (sandboxKind !== undefined) return sandboxKind;
  if (process.platform === 'darwin' && spawnSync('which', ['sandbox-exec']).status === 0) {
    sandboxKind = 'sandbox-exec';
  } else if (spawnSync('which', ['unshare']).status === 0
    && spawnSync('unshare', ['-rn', 'true']).status === 0) {
    sandboxKind = 'unshare';
  } else {
    sandboxKind = 'none';
  }
  return sandboxKind;
}

// A run directory holds the workspace (ws), a temp dir and candidates.
export function createRun(filesDir) {
  const root = realpathSync(mkdtempSync(join(tmpdir(), 'adr0038-')));
  const ws = join(root, 'ws');
  mkdirSync(join(root, 'tmp'), { recursive: true });
  mkdirSync(join(root, 'home'), { recursive: true });
  cpSync(filesDir, ws, { recursive: true });
  return { root, ws, cand: 0 };
}

export function destroyRun(run) {
  rmSync(run.root, { recursive: true, force: true });
}

export function overlay(srcDir, ws) {
  if (existsSync(srcDir)) cpSync(srcDir, ws, { recursive: true });
}

export function snapshot(run) {
  const dir = join(run.root, `cand-${run.cand++}`);
  cpSync(run.ws, dir, { recursive: true });
  return dir;
}

export function restore(run, dir) {
  rmSync(run.ws, { recursive: true, force: true });
  cpSync(dir, run.ws, { recursive: true });
}

const inside = (root, p) => p === root || p.startsWith(root + sep);

// Resolve a model-given path. Refuses paths outside the workspace, also
// through a symlink: the real path of the target (or of its nearest
// existing parent, for a new file) must stay inside the real workspace.
// A dangling symlink is refused, because a write would follow it.
export function safePath(ws, p) {
  const outside = () => new Error(`path is outside the workspace: ${p}`);
  const full = resolve(ws, String(p ?? '.'));
  if (!inside(ws, full)) throw outside();
  const root = realpathSync(ws);
  let probe = full;
  for (;;) {
    let exists = true;
    try { lstatSync(probe); } catch { exists = false; }
    if (exists) {
      let real;
      try { real = realpathSync(probe); } catch { throw outside(); }
      if (!inside(root, real)) throw outside();
      break;
    }
    const parent = dirname(probe);
    if (parent === probe) throw outside();
    probe = parent;
  }
  return full;
}

// Bounded capture of a whole stream: the first 40 % of `max` characters and
// a rolling window of the last 60 %. The end of the stream (for example a
// test summary) therefore always survives, however long the output is.
export function makeCapture(max) {
  const headMax = Math.floor(max * 0.4);
  const tailMax = max - headMax;
  let head = '';
  let tail = '';
  let total = 0;
  return {
    add(chunk) {
      let s = String(chunk);
      total += s.length;
      if (head.length < headMax) {
        const take = Math.min(headMax - head.length, s.length);
        head += s.slice(0, take);
        s = s.slice(take);
      }
      if (!s) return;
      tail += s;
      if (tail.length > tailMax * 2) tail = tail.slice(-tailMax);
    },
    text() {
      const t = tail.length > tailMax ? tail.slice(-tailMax) : tail;
      const cut = total - head.length - t.length;
      return cut > 0 ? `${head}\n... [${cut} chars cut] ...\n${t}` : head + t;
    },
  };
}

function profile(root) {
  return [
    '(version 1)',
    '(allow default)',
    '(deny network*)',
    '(allow network* (remote unix-socket))',
    '(deny file-write* (require-not (require-any',
    `  (subpath "${root}")`,
    `  (subpath "${goCache()}")`,
    '  (literal "/dev/null") (literal "/dev/tty") (subpath "/dev/fd"))))',
  ].join('\n');
}

// Run a shell command in the workspace.
// Returns {code, output, timedOut, toolFailed}. toolFailed is true only when
// the tool itself failed: a timeout, a sandbox refusal (the sandbox wrapper
// could not run the command) or a spawn error. A non-zero exit code of the
// command is not a tool failure; it is data for the model.
export function runCommand(run, command, { timeoutMs = 60_000, maxOutput = 8000 } = {}) {
  const env = {
    PATH: process.env.PATH,
    HOME: join(run.root, 'home'),
    TMPDIR: join(run.root, 'tmp'),
    GOTMPDIR: join(run.root, 'tmp'),
    GOCACHE: goCache(),
    GOPATH: join(run.root, 'home', 'go'),
    GOFLAGS: '-mod=mod',
    GOPROXY: 'off',
    GOTOOLCHAIN: 'local',
    GOTELEMETRY: 'off',
    NO_COLOR: '1',
    LANG: 'C.UTF-8',
  };
  const kind = sandboxKindAvailable();
  let file = 'sh';
  let args = ['-c', command];
  if (kind === 'sandbox-exec') {
    file = 'sandbox-exec';
    args = ['-p', profile(run.root), 'sh', '-c', command];
  } else if (kind === 'unshare') {
    file = 'unshare';
    args = ['-rn', 'sh', '-c', command];
  }
  return new Promise((done) => {
    const child = spawn(file, args, { cwd: run.ws, env, detached: true });
    const out = makeCapture(maxOutput);
    const add = (b) => out.add(b.toString());
    child.stdout.on('data', add);
    child.stderr.on('data', add);
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      try { process.kill(-child.pid, 'SIGKILL'); } catch { /* gone */ }
    }, timeoutMs);
    child.on('close', (code) => {
      clearTimeout(timer);
      const output = out.text();
      // sandbox-exec reports its own failures (for example a profile it
      // cannot apply) with a "sandbox-exec:" prefix.
      const refused = kind === 'sandbox-exec' && code !== 0 && /^sandbox-exec: /m.test(output);
      done({ code: timedOut ? 124 : (code ?? 1), output, timedOut, toolFailed: timedOut || refused });
    });
    child.on('error', (err) => {
      clearTimeout(timer);
      done({ code: 127, output: String(err), timedOut: false, toolFailed: true });
    });
  });
}

const SKIP = new Set(['node_modules', '.git']);

export function listFiles(dir, base = dir, out = [], limit = 300) {
  for (const name of readdirSync(dir).sort()) {
    if (out.length >= limit) break;
    if (SKIP.has(name)) continue;
    const full = join(dir, name);
    const st = lstatSync(full);
    if (st.isSymbolicLink()) out.push(`${relative(base, full)} (symlink, not followed)`);
    else if (st.isDirectory()) listFiles(full, base, out, limit);
    else out.push(`${relative(base, full)} (${st.size} B)`);
  }
  return out;
}

export const TOOL_SPECS = [
  {
    type: 'function',
    function: {
      name: 'read_file',
      description: 'Read a text file in the workspace. Long files are cut; use offset (line number, 1-based) and limit (lines) to page.',
      parameters: {
        type: 'object',
        properties: {
          path: { type: 'string', description: 'Path relative to the workspace root.' },
          offset: { type: 'integer' },
          limit: { type: 'integer' },
        },
        required: ['path'],
      },
    },
  },
  {
    type: 'function',
    function: {
      name: 'write_file',
      description: 'Create or overwrite a file in the workspace with the full content given.',
      parameters: {
        type: 'object',
        properties: {
          path: { type: 'string' },
          content: { type: 'string' },
        },
        required: ['path', 'content'],
      },
    },
  },
  {
    type: 'function',
    function: {
      name: 'list_files',
      description: 'List files under a directory of the workspace, recursively.',
      parameters: {
        type: 'object',
        properties: { path: { type: 'string', description: 'Directory, default the root.' } },
      },
    },
  },
  {
    type: 'function',
    function: {
      name: 'run_command',
      description: 'Run a shell command in the workspace root. No network. Time limit 60 s. Output is cut to 8000 characters. Returns the exit code and the output.',
      parameters: {
        type: 'object',
        properties: { command: { type: 'string' } },
        required: ['command'],
      },
    },
  },
];

const MAX_READ = 20_000;

// Execute one tool call. Returns {ok, text}. ok=false means the tool failed
// (bad arguments, a refused path, a command timeout, a sandbox refusal, a
// spawn error). For run_command the exit code is data in the text: a
// non-zero exit of a routine probe is not a tool failure.
export async function execTool(run, name, rawArgs) {
  let args;
  try {
    args = typeof rawArgs === 'string' ? JSON.parse(rawArgs || '{}') : (rawArgs ?? {});
  } catch (err) {
    return { ok: false, text: `error: arguments are not valid JSON: ${err.message}` };
  }
  try {
    switch (name) {
      case 'read_file': {
        const p = safePath(run.ws, args.path);
        let text = readFileSync(p, 'utf8');
        if (args.offset || args.limit) {
          const lines = text.split('\n');
          const start = Math.max(0, (args.offset ?? 1) - 1);
          text = lines.slice(start, start + (args.limit ?? lines.length)).join('\n');
        }
        if (text.length > MAX_READ) {
          text = `${text.slice(0, MAX_READ)}\n... [cut: ${text.length - MAX_READ} more chars; use offset/limit]`;
        }
        return { ok: true, text };
      }
      case 'write_file': {
        const p = safePath(run.ws, args.path);
        if (typeof args.content !== 'string') throw new Error('content must be a string');
        mkdirSync(dirname(p), { recursive: true });
        writeFileSync(p, args.content);
        return { ok: true, text: `wrote ${args.content.length} chars to ${args.path}` };
      }
      case 'list_files': {
        const p = safePath(run.ws, args.path ?? '.');
        const files = listFiles(p);
        return { ok: true, text: files.join('\n') || '(empty)' };
      }
      case 'run_command': {
        if (typeof args.command !== 'string' || !args.command.trim()) {
          throw new Error('command must be a non-empty string');
        }
        const r = await runCommand(run, args.command);
        const head = r.timedOut ? 'timed out after 60 s (exit 124)' : `exit code ${r.code}`;
        return { ok: !r.toolFailed, text: `${head}\n${r.output}` };
      }
      default:
        return { ok: false, text: `error: unknown tool ${name}` };
    }
  } catch (err) {
    return { ok: false, text: `error: ${err.message}` };
  }
}
