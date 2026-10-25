import hashlib
import json
import os
from pathlib import Path
import resource
import shutil
import signal
import subprocess
import sys
import time

source = Path(sys.argv[1]).resolve()
evidence = Path(sys.argv[2]).resolve()
evidence.mkdir(parents=True, exist_ok=True)
env = dict(os.environ)
for key in ('HERDR_ENV', 'GH_TOKEN', 'GITHUB_TOKEN', 'GH_CONFIG_DIR', 'CLAUDE_CODE_OAUTH_TOKEN', 'ANTHROPIC_API_KEY', 'OPENAI_API_KEY'):
    env.pop(key, None)
env.update(GOTOOLCHAIN='local', GOENV='off', GOMAXPROCS='2', CGO_ENABLED='0', GOWORK='off')

def capture(args):
    return subprocess.run(args, cwd=source, env=env, check=True, capture_output=True, text=True).stdout.strip()

head = capture(['git', 'rev-parse', 'HEAD'])
assert head == env['TARGET_SHA']
provenance = {
    'source_head': head,
    'source_tree': capture(['git', 'rev-parse', 'HEAD^{tree}']),
    'go': capture(['go', 'version']),
    'instrument_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
    'run_id': env.get('GITHUB_RUN_ID'),
    'run_attempt': env.get('GITHUB_RUN_ATTEMPT'),
    'runner_image': env.get('ImageVersion'),
}
# Model the native seat's existing daemon-prefetched module cache. This preparatory
# download is separate from, and excluded from, the offline private-cache peak.
with (evidence / 'prepare-modules.stdout').open('w') as out, (evidence / 'prepare-modules.stderr').open('w') as err:
    subprocess.run(['go', 'mod', 'download'], cwd=source, env=env, stdout=out, stderr=err, check=True, timeout=300)
module_cache = capture(['go', 'env', 'GOMODCACHE'])
module_size = capture(['du', '-sb', module_cache]).split()[0]
private = evidence.parent / 'cold-focused-cache'
assert not private.exists()
for name in ('build', 'tmp', 'home'):
    (private / name).mkdir(parents=True, exist_ok=True)
env.update(GOCACHE=str(private / 'build'), TMPDIR=str(private / 'tmp'), HOME=str(private / 'home'),
           GOMODCACHE=module_cache, GOPROXY='off', GOSUMDB='off', GOVCS='*:off',
           HERDR_SOCKET_PATH=str(private / 'herdr-disabled'))

def space():
    st = os.statvfs(private)
    return st.f_bavail * st.f_frsize, int(st.f_blocks * st.f_frsize * 0.05)

def allocated():
    total = 0
    for directory, _, names in os.walk(private):
        for name in names:
            try:
                total += (Path(directory) / name).stat().st_blocks * 512
            except FileNotFoundError:
                pass
    return total

initial_free, floor = space()
assert initial_free >= floor + 512 * 1024**2
command = ['go', 'test', '-c', '-tags', 'e2e', '-p', '1', '-o', str(private / 'workflow.test'), './internal/workflow']
receipt = {'provenance': provenance, 'command': command, 'cwd': str(source),
           'module_cache_bytes': int(module_size), 'initial_private_bytes': allocated(),
           'dependency_mode': 'offline prefilled GOMODCACHE; fresh empty GOCACHE and TMPDIR',
           'initial_free': initial_free, 'floor': floor, 'samples': []}
minimum_free, maximum_private = initial_free, 0
started = time.monotonic()
with (evidence / 'cold-build.stdout').open('w') as out, (evidence / 'cold-build.stderr').open('w') as err:
    process = subprocess.Popen(command, cwd=source, env=env, stdout=out, stderr=err, start_new_session=True)
    while process.poll() is None:
        free, current_floor = space()
        size = allocated()
        minimum_free = min(minimum_free, free)
        maximum_private = max(maximum_private, size)
        receipt['samples'].append({'seconds': round(time.monotonic() - started, 3), 'free': free, 'private_allocated': size})
        if free < current_floor + 512 * 1024**2 or time.monotonic() - started > 900:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            receipt['interrupted'] = 'capacity or 900s deadline'
            break
        time.sleep(1)
    receipt['exit'] = process.wait()
receipt.update(seconds=round(time.monotonic() - started, 3), minimum_free=minimum_free,
               observed_volume_growth=initial_free - minimum_free, peak_private_allocated=maximum_private,
               final_private_allocated=allocated(), max_child_rss_kib=resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss)
if receipt['exit'] == 0:
    binary = private / 'workflow.test'
    receipt['binary_bytes'] = binary.stat().st_size
    receipt['binary_sha256'] = hashlib.sha256(binary.read_bytes()).hexdigest()
(evidence / 'cold-build.json').write_text(json.dumps(receipt, indent=2) + '\n')
assert receipt['exit'] == 0, receipt
# Exercise the selected behavior once with the exact measured build; no full suite.
check = [str(private / 'workflow.test'), '-test.v', '-test.timeout=120s',
         '-test.run=^TestCleanupRetainsServiceProducerWithoutConsumerEvidence$']
with (evidence / 'focused-check.stdout').open('w') as out, (evidence / 'focused-check.stderr').open('w') as err:
    result = subprocess.run(check, cwd=source, env=env, stdout=out, stderr=err, timeout=150)
(evidence / 'focused-check.json').write_text(json.dumps({'command': check, 'exit': result.returncode, 'source_head': head}, indent=2) + '\n')
assert result.returncode == 0
shutil.rmtree(private)
