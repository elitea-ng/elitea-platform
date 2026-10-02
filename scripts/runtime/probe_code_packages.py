"""Run trusted language fixtures. This does not prove sandbox isolation."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile


def run(command, cwd, env):
    process = subprocess.Popen(command, cwd=cwd, env=env, text=True,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               start_new_session=True)
    try:
        stdout, stderr = process.communicate(timeout=120)
        if process.returncode:
            raise RuntimeError(f'{command[0]} failed ({process.returncode}): {stderr}')
        return stdout
    finally:
        # Own the compiler/runtime process group, including timeout cleanup.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cache', type=Path, required=True)
    parser.add_argument('--prepare', action='store_true',
                        help='Permit package downloads; otherwise require cached packages')
    args = parser.parse_args()
    cache = args.cache.resolve()
    cache.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, DENO_DIR=str(cache / 'deno'),
               CARGO_TARGET_DIR=str(cache / 'target'), CARGO_BUILD_JOBS='2')
    with tempfile.TemporaryDirectory(prefix='elitea-code-packages-') as directory:
        workspace = Path(directory)
        for language, extension, annotation in [('javascript', 'js', ''),
                                                ('typescript', 'ts', ': number')]:
            source = workspace / f'code.{extension}'
            source.write_text(
                'import semver from "npm:semver@7.7.2";\n'
                f'const count{annotation} = 2;\n'
                'console.log(JSON.stringify({count: count + 1, '
                'compatible: semver.satisfies("2.5.4", ">=2.0.0")}));\n'
            )
            command = ['deno', 'run', '--no-prompt', '--no-config', '--no-lock', '--deny-net']
            if not args.prepare:
                command.append('--cached-only')
            result = json.loads(run([*command, str(source)], workspace, env))
            assert result == {'count': 3, 'compatible': True}, result
            print(json.dumps({'language': language, 'result': result}), flush=True)

        # Use stable Cargo. No custom parser or nightly embedded-manifest format.
        (workspace / 'Cargo.toml').write_text(
            '[package]\nname = "elitea-code-package-probe"\nversion = "0.0.0"\n'
            'edition = "2021"\n[dependencies]\nserde_json = "=1.0.151"\n'
            '[[bin]]\nname = "probe"\npath = "code.rs"\n'
        )
        (workspace / 'code.rs').write_text('''
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let state: serde_json::Value = serde_json::from_str(r#"{"count":2}"#)?;
    println!("{}", serde_json::json!({"count": state["count"].as_i64().ok_or("count")? + 1}));
    Ok(())
}
''')
        offline = [] if args.prepare else ['--offline']
        run(['cargo', 'generate-lockfile', *offline], workspace, env)
        result = json.loads(run(['cargo', 'run', '--quiet', '--locked', *offline], workspace, env))
        assert result == {'count': 3}, result
        # The same source and lockfile must work without another registry request.
        assert json.loads(run(['cargo', 'run', '--quiet', '--frozen'], workspace, env)) == result
        print(json.dumps({'language': 'rust', 'result': result, 'frozen_replay': True}), flush=True)


if __name__ == '__main__':
    main()
