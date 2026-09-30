#!/usr/bin/env python3
"""Isolated live supervisor test; fixture signer is not Main authorization proof."""
import argparse, base64, hashlib, json, pathlib, socket, subprocess, sys, time, uuid
import grpc
from cryptography import x509
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'libs/proto/gen/python'))
from elitea.runtime.v1 import sandbox_pb2 as pb, sandbox_pb2_grpc as rpc
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--context', required=True)
parser.add_argument('--namespace', required=True)
parser.add_argument('--worker-material', required=True, type=pathlib.Path)
args = parser.parse_args()
CTX = args.context
NS = args.namespace
NAME = 'elitea-recovery-test-' + uuid.uuid4().hex[:10]
if NS == 'default' or NS.startswith('kube-'):
    parser.error('use a dedicated rehearsal platform namespace')
with socket.socket() as listener:
    listener.bind(('127.0.0.1', 0))
    local_port = listener.getsockname()[1]

def run(args, data=None, timeout=120):
    r = subprocess.run(args, input=data, capture_output=True, timeout=timeout)
    if r.returncode:
        raise RuntimeError('Operation failed: ' + args[0] + ' ' + args[1])
    return r.stdout

def kub(*args, data=None):
    return run(['kubectl', '--context', CTX, '-n', NS, *args], data)

def apply(d):
    return kub('apply', '-f', '-', data=json.dumps(d).encode())

def read(name):
    return (args.worker_material / name).read_bytes()

def js(d):
    return json.dumps(d, separators=(',', ':'), ensure_ascii=False).encode()
materials = json.loads(kub('get', 'secret', 'sandbox-material', '-o', 'json'))['data']
config = json.loads(base64.b64decode(materials['deno.json']))
config['owner'] = NAME
config['timeout_seconds'] = 120
config['verification_keyring_path'] = '/run/elitea-sandbox/fixture-keyring.json'
key = Ed25519PrivateKey.generate()
materials['fixture-keyring.json'] = base64.b64encode(js({'schema_version': 'elitea.runtime-ed25519-keyring.v1', 'keys': [{'key_id': 'isolated-fixture', 'public_key_base64': base64.b64encode(key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)).decode()}]})).decode()
materials['deno.json'] = base64.b64encode(js(config)).decode()
apply({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': NAME, 'namespace': NS}, 'type': 'Opaque', 'data': materials})
deployment = json.loads(kub('get', 'deployment', 'elitea-sandbox-supervisor', '-o', 'json'))
labels = {'app': NAME}
deployment = {'apiVersion': 'apps/v1', 'kind': 'Deployment', 'metadata': {'name': NAME, 'namespace': NS}, 'spec': deployment['spec']}
deployment['spec']['selector'] = {'matchLabels': labels}
deployment['spec']['template']['metadata'] = {'labels': labels, 'annotations': {'test-revision': uuid.uuid4().hex}}
spec = deployment['spec']['template']['spec']
spec['volumes'][0]['secret']['secretName'] = NAME
spec['containers'][0]['args'] = ['--config', '/run/elitea-sandbox/deno.json']
spec['containers'][0]['ports'] = [{'name': 'profile-0', 'containerPort': 9446}]
apply(deployment)
kub('rollout', 'status', 'deployment/' + NAME, '--timeout=60s')
cert = read('agent-worker-client.crt')
clientkey = read('agent-worker-client.key')
ca = read('runtime-ca.crt')
sans = x509.load_pem_x509_certificate(cert).extensions.get_extension_for_class(x509.SubjectAlternativeName).value
dns = sans.get_values_for_type(x509.DNSName)
peer = 'dns:' + dns[0] if dns else sans.get_values_for_type(x509.UniformResourceIdentifier)[0]
creds = grpc.ssl_channel_credentials(ca, clientkey, cert)
forward = None

def connect():
    global forward
    if forward:
        forward.terminate()
        forward.wait(timeout=10)
    forward = subprocess.Popen(['kubectl', '--context', CTX, '-n', NS, 'port-forward', 'deployment/' + NAME, f'{local_port}:9446'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    channel = grpc.secure_channel(f'localhost:{local_port}', creds, options=[('grpc.ssl_target_name_override', config['audience'].removeprefix('dns:'))])
    grpc.channel_ready_future(channel).result(timeout=20)
    return (channel, rpc.SandboxSupervisorServiceStub(channel))
job = {'revision': 1, 'language': 'javascript', 'source': 'export default async () => { const marker = crypto.randomUUID(); await new Promise(resolve => setTimeout(resolve, 45000)); return {marker, status:"PASS"}; };', 'input': {}, 'image_digest': config['image_digest'], 'policy_revision': config['policy_revision'], 'timeout_seconds': 90}
prepared = js(job)
digest = hashlib.sha256(b'elitea.sandbox.prepared-job.v1\x00' + len(prepared).to_bytes(8, 'big') + prepared).digest()
execution = 'kube-recovery-' + uuid.uuid4().hex

def request(peer_override=None):
    now = int(time.time() * 1000)
    claims = pb.SandboxJobGrantClaimsV1(revision=1, tenant_id='isolated-kubernetes-test', project_id=2, execution_id=execution, activation_id='code-1', request_digest=digest, submitter_workload_identity=peer_override or peer, audience=config['audience'], issued_at_unix_millis=now, expires_at_unix_millis=now + 30000, generation=1)
    payload = claims.SerializeToString()
    signed = b'elitea.sandbox.job-grant.ed25519.v1\x00' + len(payload).to_bytes(8, 'big') + payload
    return pb.SubmitSandboxJobRequestV1(grant=pb.SignedSandboxJobGrantV1(key_id='isolated-fixture', claims_bytes=payload, signature=key.sign(signed)), prepared_job_json=prepared)
try:
    channel, stub = connect()
    try:
        stub.SubmitSandboxJob(request('dns:wrong-worker'), timeout=10)
        raise AssertionError('wrong peer accepted')
    except grpc.RpcError as e:
        assert e.code() == grpc.StatusCode.PERMISSION_DENIED, e.code()
    print('PASS: signed grant with wrong mTLS peer rejected', flush=True)
    future = stub.SubmitSandboxJob.future(request(), timeout=110)
    deadline = time.monotonic() + 30
    execution_pod = None
    while time.monotonic() < deadline:
        pods = json.loads(run(['kubectl', '--context', CTX, '-n', config['backend']['namespace'], 'get', 'pods', '-o', 'json']))['items']
        matches = [p for p in pods if p['metadata'].get('annotations', {}).get('sandbox.elitea.ai/request') == digest.hex()]
        if matches:
            execution_pod = matches[0]
            check = subprocess.run(['kubectl', '--context', CTX, '-n', config['backend']['namespace'], 'exec', execution_pod['metadata']['name'], '--', 'test', '-f', '/workspace/.elitea-dispatch'], capture_output=True)
            if check.returncode == 0:
                break
        time.sleep(0.3)
    else:
        raise AssertionError('dispatch marker not observed')
    uid = execution_pod['metadata']['uid']
    name = execution_pod['metadata']['name']
    old = json.loads(kub('get', 'pods', '-l', 'app=' + NAME, '-o', 'json'))['items'][0]['metadata']['name']
    kub('delete', 'pod', old, '--grace-period=1', '--wait=true')
    kub('rollout', 'status', 'deployment/' + NAME, '--timeout=60s')
    channel.close()
    channel, stub = connect()
    observed = json.loads(run(['kubectl', '--context', CTX, '-n', config['backend']['namespace'], 'get', 'pod', name, '-o', 'json']))
    assert observed['metadata']['uid'] == uid
    print('PASS: supervisor replaced; original execution Pod UID retained', flush=True)
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        result = stub.SubmitSandboxJob(request(), timeout=110)
        if result.status != pb.SANDBOX_JOB_STATUS_V1_PENDING:
            break
        time.sleep(1)
    assert result.status == pb.SANDBOX_JOB_STATUS_V1_COMPLETED, (result.status, result.failure_code)
    again = stub.SubmitSandboxJob(request(), timeout=20)
    assert again.result_json == result.result_json and again.status == result.status
    print('PASS: same activation recovered to completed; repeated receipt is identical', flush=True)
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        pods = json.loads(run(['kubectl', '--context', CTX, '-n', config['backend']['namespace'], 'get', 'pods', '-o', 'json']))['items']
        if not any((p['metadata']['name'] == name for p in pods)):
            break
        time.sleep(0.3)
    else:
        raise AssertionError('receipt workload not cleaned')
    print('PASS: execution Pod cleaned after durable completion', flush=True)
    kub('delete', 'deployment', NAME, '--wait=true')
    kub('delete', 'secret', NAME)
    print('Removed isolated test supervisor and fixture signing material', flush=True)
finally:
    if forward:
        forward.terminate()
        forward.wait(timeout=10)
