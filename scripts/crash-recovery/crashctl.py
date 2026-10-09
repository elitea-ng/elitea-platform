#!/usr/bin/env python3
"""Crash-recovery acceptance harness for the replatform (DESIGN: handoffs/crash-recovery-suite/DESIGN.md).

Standard library only. Local and opt-in: CI never runs it. Subcommands:
  init                 write the private stack config template (stack.json, mode 600)
  material             create certificates, Supervisor material and the rendered Worker config
  up                   restore the real-model dump and start the dedicated compose crash stack
  seed                 create fixtures through the public API (records fixtures.json)
  preflight            run the scenario preflight once and print it
  run SCOPE...         run scenarios (ids, or a tier name such as T0); writes evidence and verdicts
  verify RUN_ID        re-evaluate the verdicts of a run from its stored evidence
  report RUN_ID...     summarize runs: per-scenario verdicts, measurements, matrix-cell proposals
  binary-check         §3b image identity check: extract each evidence binary and look for a marker
  down [--volumes]     stop the crash stack (only the crash project; never another stack)
"""
import argparse
import json
import pathlib
import sys
import time

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

from lib import api, catalog, report  # noqa: E402
from lib.common import DEFAULT_RUNS, HarnessError, iso, read_json, write_json  # noqa: E402
from lib.redact import scan_tree  # noqa: E402
from lib.runner import preflight, run_scenario  # noqa: E402
from lib.collectors import Collector  # noqa: E402
from lib.stack_compose import Stack  # noqa: E402
from lib.verdict import evaluate  # noqa: E402
from fixtures import seed as seed_mod  # noqa: E402

DEFAULT_PYTHON = '/Users/romanmitusov/Documents/Work/.claude/projects/venv/bin/python3'


def _client(stack):
    client = api.Client(stack.base_url)
    client.login_oidc(stack.cfg['oidc_subject'])
    return client


def cmd_init(args):
    print(Stack.init(args.stack_dir))


def cmd_material(args):
    stack = Stack(args.stack_dir)
    print(json.dumps(stack.prepare_material(args.python), indent=2))


def cmd_up(args):
    stack = Stack(args.stack_dir)
    print(stack.restore_dump())
    stack.up()
    state = stack.state()
    state.setdefault('baseline_at', iso())
    stack.save_state(state)
    print(f'up: {stack.base_url}/app/ baseline_at={state["baseline_at"]}')


def cmd_seed(args):
    stack = Stack(args.stack_dir)
    client = _client(stack)
    token_uuid = client.mint_token(f'crash-seed-{int(time.time())}')
    try:
        project_id = args.project or client.author()['personal_project_id']
        fixtures = seed_mod.seed(client, int(project_id), stack.base_url, stack.dir / 'fixtures.json',
                                 time.strftime('%Y%m%d%H%M%S'))
    finally:
        client.token = None
        client.revoke_token(token_uuid)
    print(json.dumps({k: v for k, v in fixtures.items()}, indent=2))


def cmd_preflight(args):
    stack = Stack(args.stack_dir)
    print(json.dumps(preflight(stack, Collector(stack)), indent=2, default=str))


def cmd_run(args):
    stack = Stack(args.stack_dir)
    fixtures = read_json(stack.dir / 'fixtures.json')
    if not fixtures:
        raise HarnessError('no fixtures.json; run `crashctl seed`')
    scenarios = catalog.select(args.scope)
    oracle = read_json(stack.dir / 'oracle.json', {})
    run_id = args.run_id or time.strftime('%Y%m%dT%H%M%S')
    run_dir = pathlib.Path(args.runs_dir) / run_id
    client = _client(stack)
    summary = read_json(run_dir / 'summary.json', {'run_id': run_id, 'started': iso(), 'results': []})
    for scenario in scenarios:
        scenario['_oracle'] = oracle.get(scenario['fixture'], {})
        repeat = args.repeat or scenario.get('repeat', 1)
        for index in range(args.first_repeat, args.first_repeat + repeat):
            browser = args.browser and scenario.get('browser', False) and index == args.first_repeat
            out = run_dir / scenario['id'] / f'r{index}'
            print(f"{iso()} {scenario['id']} r{index} start (browser={browser})", flush=True)
            rec, result = run_scenario(stack, client, scenario, fixtures, out, browser=browser)
            line = {'scenario': scenario['id'], 'repeat': index, 'execution_id': rec.get('execution_id'),
                    'verdict': result['verdict'], 'observed': result.get('observed'), 'target': scenario['target'],
                    'why': result.get('why') or result.get('reason'), 'dir': str(out.relative_to(run_dir))}
            summary['results'].append(line)
            write_json(run_dir / 'summary.json', summary)
            print(f"{iso()} {scenario['id']} r{index} {result['verdict']} observed={result.get('observed')} "
                  f"exec={rec.get('execution_id')} {line['why']}", flush=True)
            if scenario['id'].startswith('BASE-') and result['verdict'] == 'PASS':
                oracle[scenario['fixture']] = {'jobs': len(rec['final']['agentstate']['sandbox'] or []),
                                               'execution_id': rec['execution_id']}
                write_json(stack.dir / 'oracle.json', oracle)
            client = _client(stack)  # a Main restart ends nothing, but a fresh session keeps runs independent
    hits = scan_tree(run_dir)
    summary['redaction_scan'] = {'files_flagged': hits}
    write_json(run_dir / 'summary.json', summary)
    if hits:
        raise HarnessError(f'redaction scan flagged {len(hits)} files in {run_dir}')


def cmd_verify(args):
    run_dir = pathlib.Path(args.runs_dir) / args.run_id
    stack_oracle = read_json(Stack(args.stack_dir).dir / 'oracle.json', {})
    for evidence in sorted(run_dir.glob('*/r*/evidence.json')):
        rec = read_json(evidence)
        scenario = catalog.load(rec['scenario'])
        scenario['_oracle'] = stack_oracle.get(scenario['fixture'], {})
        result = evaluate(scenario, rec)
        write_json(evidence.with_name('verdict.json'), result)
        print(rec['scenario'], evidence.parent.name, result['verdict'], result.get('observed'), result.get('why'))


def cmd_report(args):
    out = report.build([pathlib.Path(args.runs_dir) / r for r in args.run_ids])
    path = pathlib.Path(args.runs_dir) / args.run_ids[-1] / 'report.json'
    write_json(path, out)
    print(report.markdown(out))


def cmd_binary_check(args):
    print(json.dumps(report.binary_check(json.loads(pathlib.Path(args.spec).read_text())), indent=2))


def cmd_down(args):
    Stack(args.stack_dir).down(volumes=args.volumes)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--stack-dir', default=None)
    parser.add_argument('--runs-dir', default=str(DEFAULT_RUNS))
    sub = parser.add_subparsers(dest='cmd', required=True)
    sub.add_parser('init').set_defaults(fn=cmd_init)
    p = sub.add_parser('material')
    p.add_argument('--python', default=DEFAULT_PYTHON, help='a python3 with `cryptography`, for `standalone-stack.sh certs`')
    p.set_defaults(fn=cmd_material)
    sub.add_parser('up').set_defaults(fn=cmd_up)
    p = sub.add_parser('seed')
    p.add_argument('--project', type=int, default=None)
    p.set_defaults(fn=cmd_seed)
    sub.add_parser('preflight').set_defaults(fn=cmd_preflight)
    p = sub.add_parser('run')
    p.add_argument('scope', nargs='+')
    p.add_argument('--repeat', type=int, default=None)
    p.add_argument('--run-id', default=None)
    p.add_argument('--first-repeat', type=int, default=1, help='index of the first repeat (continue a run)')
    p.add_argument('--browser', action='store_true')
    p.set_defaults(fn=cmd_run)
    p = sub.add_parser('verify')
    p.add_argument('run_id')
    p.set_defaults(fn=cmd_verify)
    p = sub.add_parser('report')
    p.add_argument('run_ids', nargs='+')
    p.set_defaults(fn=cmd_report)
    p = sub.add_parser('binary-check')
    p.add_argument('spec', help='JSON: [{"image": ref, "path": in-image binary, "marker": string, "expect": true}]')
    p.set_defaults(fn=cmd_binary_check)
    p = sub.add_parser('down')
    p.add_argument('--volumes', action='store_true')
    p.set_defaults(fn=cmd_down)
    args = parser.parse_args()
    try:
        args.fn(args)
    except HarnessError as err:
        print(f'crashctl: {err}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
