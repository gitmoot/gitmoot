import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time

source = Path(sys.argv[1]).resolve()
evidence = Path(sys.argv[2]).resolve()
mode = sys.argv[3]
instruments = Path(__file__).resolve().parent
evidence.mkdir(parents=True, exist_ok=True)
env = dict(os.environ)
for key in ('HERDR_ENV', 'GITMOOT_ORG_ROLE', 'HERDR_PANE_ID', 'GH_TOKEN', 'GITHUB_TOKEN',
            'GH_CONFIG_DIR', 'CLAUDE_CODE_OAUTH_TOKEN', 'ANTHROPIC_API_KEY', 'OPENAI_API_KEY'):
    env.pop(key, None)
env.update(GOTOOLCHAIN='local', GOMAXPROCS='2',
           HERDR_SOCKET_PATH=str(evidence / 'herdr-disabled'),
           XDG_STATE_HOME=str(evidence / 'state'),
           GIT_CONFIG_COUNT='1', GIT_CONFIG_KEY_0='safe.directory', GIT_CONFIG_VALUE_0=str(source))

def text(command):
    return subprocess.check_output(command, cwd=source, env=env, text=True).strip()

head = text(['git', 'rev-parse', 'HEAD'])
assert head == env['TARGET_SHA'], (head, env['TARGET_SHA'])
provenance = {
    'target_head': head, 'source_tree': text(['git', 'rev-parse', 'HEAD^{tree}']),
    'instrument_head': os.environ.get('GITHUB_SHA'), 'platform': platform.platform(),
    'uid': os.geteuid(), 'image': os.environ.get('ImageVersion'),
    'mode': mode, 'initial_status': text(['git', 'status', '--porcelain']),
    'go': text(['go', 'version']),
    'started_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
    'instrument_hashes': {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                          for p in instruments.iterdir() if p.is_file()},
}
assert not provenance['initial_status'], provenance['initial_status']
(evidence / 'provenance.json').write_text(json.dumps(provenance, indent=2) + '\n')

volumes = {}
for path in (source, evidence, Path(tempfile.gettempdir())):
    volumes.setdefault(path.stat().st_dev, path)

def spaces():
    result = []
    for device, path in volumes.items():
        stat = os.statvfs(path)
        result.append({'device': device, 'path': str(path),
                       'available': stat.f_bavail * stat.f_frsize,
                       'floor': int(stat.f_blocks * stat.f_frsize * 0.05)})
    return result


def run(name, command, cwd=source, allowed=(0,), timeout=1800):
    stdout = evidence / (name + '.stdout')
    stderr = evidence / (name + '.stderr')
    receipt_file = evidence / (name + '.json')
    assert not stdout.exists() and not stderr.exists() and not receipt_file.exists()
    receipt = {'command': command, 'cwd': str(cwd), 'source_head': head,
               'started_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
               'space_before': spaces(), 'stdout': stdout.name, 'stderr': stderr.name}
    if any(row['available'] < row['floor'] + 512 * 1024**2 for row in receipt['space_before']):
        receipt['blocked'] = 'Below unchanged 5% floor plus 512 MiB start margin'
        receipt_file.write_text(json.dumps(receipt, indent=2) + '\n')
        raise RuntimeError(receipt)
    minimum = {row['device']: row['available'] for row in receipt['space_before']}
    started = time.monotonic()
    interrupted = None
    with stdout.open('x') as out, stderr.open('x') as err:
        process = subprocess.Popen(command, cwd=cwd, env=env, stdout=out, stderr=err,
                                   start_new_session=True)
        while process.poll() is None:
            current = spaces()
            for row in current:
                minimum[row['device']] = min(minimum[row['device']], row['available'])
            if interrupted is None:
                low_space = any(row['available'] < row['floor'] + 128 * 1024**2 for row in current)
                if low_space or time.monotonic() - started >= timeout:
                    receipt['interrupted'] = 'disk_guard' if low_space else 'command_timeout'
                    receipt['interruption_space'] = current
                    os.killpg(process.pid, signal.SIGINT)
                    interrupted = time.monotonic()
            elif time.monotonic() - interrupted >= 30:
                os.killpg(process.pid, signal.SIGKILL)
            time.sleep(2)
        receipt['exit'] = process.returncode
    receipt.update(seconds=round(time.monotonic() - started, 3),
                   minimum_available=minimum, space_after=spaces())
    receipt_file.write_text(json.dumps(receipt, indent=2) + '\n')
    print(json.dumps(receipt), flush=True)
    if interrupted is not None or process.returncode not in allowed:
        print(stdout.read_text()[-16000:], flush=True)
        print(stderr.read_text()[-16000:], file=sys.stderr, flush=True)
        raise RuntimeError('check failed: ' + name)
    return stdout


if mode == 'smoke':
    assert os.geteuid() == 0, 'real process and mount-namespace visibility requires root'
    binary = evidence / 'gitmoot'
    package = 'github.com/gitmoot/gitmoot/internal/buildinfo'
    stamp = datetime.datetime.now(datetime.timezone.utc).isoformat()
    flags = f'-X {package}.Version=verify-{head[:12]} -X {package}.Commit={head} -X {package}.Date={stamp}'
    run('build-native', ['go', 'build', '-p', '1', '-trimpath', '-ldflags', flags,
                         '-o', str(binary), './cmd/gitmoot'], timeout=900)
    run('native-version', [str(binary), 'version'], timeout=30)
    binary_hashes = {'gitmoot': hashlib.sha256(binary.read_bytes()).hexdigest()}
    for kind in ('cli', 'worker'):
        directory = source / f'.cleanup-{kind}-smoke'
        assert not directory.exists()
        directory.mkdir()
        script = instruments / f'cleanup-{kind}-smoke.go'
        destination = directory / 'main.go'
        shutil.copyfile(script, destination)
        assert destination.read_bytes() == script.read_bytes()
        driver = evidence / f'{kind}-smoke'
        try:
            run(f'build-{kind}-driver', ['go', 'build', '-p', '1', '-o', str(driver),
                                         f'./.cleanup-{kind}-smoke'], timeout=900)
            binary_hashes[kind] = hashlib.sha256(driver.read_bytes()).hexdigest()
            run(f'native-{kind}-smoke', [str(driver), str(binary)], timeout=180)
        finally:
            shutil.rmtree(directory)
            driver.unlink(missing_ok=True)
    (evidence / 'binary-hashes.json').write_text(json.dumps(binary_hashes, indent=2) + '\n')
    binary.unlink()
elif mode == 'proofs':
    package = env['PROOF_PACKAGE']
    assert package in ('cli', 'db', 'workflow')
    base = {'original': '574d0705976d280fb5ad6fcacbfef6b5ddb27514',
            'corrections': '999c965ff914592aca4dd02f82c53127a7070b9f',
            'review': '6c18b98c7469eddd7729ca2c8e4df0b4bba9ba2d'}[env['PROOF_BASE']]
    tool_revision = 'd5f169ab623e8ba0896b727a09f36738612a79e2'
    tools = evidence / 'tools'
    tools.mkdir()
    env['GOBIN'] = str(tools)
    run('install-test-check', ['go', 'install',
        'github.com/gitmoot/test-check/cmd/test-check@' + tool_revision], timeout=900)
    proof = str(tools / 'test-check')
    run('test-check-provenance', ['go', 'version', '-m', proof], timeout=30)
    listing = run('changed-tests', [proof, 'prove', '--base', base, '--list-tests'], timeout=60)
    changed = set(listing.read_text().splitlines())
    definitions = set()
    for path in (source / 'internal' / package).glob('*_test.go'):
        definitions.update(re.findall(r'^func\s+(Test\w+)\s*\(', path.read_text(), re.MULTILINE))
    names = sorted(changed & definitions)
    excluded = []
    if 'TestCleanupRetryProcessHelper' in names:
        names.remove('TestCleanupRetryProcessHelper')
        excluded.append({'name': 'TestCleanupRetryProcessHelper',
                         'reason': 'Executed by TestCleanupRetryWaitsAcrossProcessBeforeReadingWorkspace; not standalone'})
    assert names, (package, base, sorted(changed))
    (evidence / 'selection.json').write_text(json.dumps(
        {'package': package, 'base': base, 'tests': names, 'excluded': excluded}, indent=2) + '\n')
    command = f"go test -tags e2e -count=1 -p 1 ./internal/{package} -v -timeout 10m -run '^{{name}}$'"
    output = run('prove', [proof, 'prove', '--base', base, '--each', ','.join(names),
                           '--test', command, '--json'], allowed=(0, 4), timeout=5400)
    report = json.loads(output.read_text())
    results = report.get('tests', [])
    assert {row['name'] for row in results} == set(names), report
    for row in results:
        assert row['outcome'] in ('proven', 'inconclusive', 'not_red_on_old'), row
        assert re.search(r'^--- PASS: ' + re.escape(row['name']) + r'\b',
                         row.get('new_output', ''), re.MULTILINE), row
    shutil.rmtree(tools)
elif mode == 'mutations':
    cases = [
        ('undispatched', 'workflow', 'TestCleanupPreservesResumableConsumerUntilSucceeded',
         'internal/workflow/cleanup_contract.go',
         'return "consumer_undispatched", nil',
         '/* mutation: accept undispatched consumers */',
         'undispatched consumer lost producer contents'),
        ('runtime-query', 'workflow', 'TestCleanupRefusesUnreadableRuntimeOwner',
         'internal/workflow/cleanup_preservation.go',
         '\tif err != nil {\n\t\treturn "runtime_owner_unknown"\n\t}\n\tif liveness != nil',
         '\tif err != nil {\n\t\treturn ""\n\t}\n\tif liveness != nil',
         'unreadable runtime owner was not refused'),
        ('collector', 'cli', 'TestCleanupPublishesForBlockedPipelineAndRefusesLostArtifacts',
         'internal/cli/ask_diff_precleanup.go',
         'errs = append(errs, err)', 'return err',
         'artifact failure prevented durable diff collection'),
        ('legacy-event', 'db', 'TestJobIDsWithAgedTerminalDelegationWorktree',
         'internal/db/store_jobs.go',
         'AND (j.resource_id IS NOT NULL OR NOT EXISTS (',
         'AND (1 = 1 OR NOT EXISTS (',
         'legacy-cleanup'),
    ]
    results = []
    for label, package, test, relative, before, after, expected_failure in cases:
        command = ['go', 'test', '-tags', 'e2e', '-count=1', '-p', '1',
                   './internal/' + package, '-v', '-timeout', '10m', '-run', '^' + test + '$']
        baseline = run(label + '-baseline', command, timeout=900).read_text()
        assert re.search(r'^--- PASS: ' + re.escape(test) + r'\b', baseline, re.MULTILINE), baseline
        path = source / relative
        original = path.read_bytes()
        content = original.decode()
        assert content.count(before) == 1, (relative, before, content.count(before))
        mutant = content.replace(before, after).encode()
        record = {'test': test, 'file': relative, 'source_head': head,
                  'original_sha256': hashlib.sha256(original).hexdigest(),
                  'mutant_sha256': hashlib.sha256(mutant).hexdigest(),
                  'before': before, 'after': after, 'expected_failure': expected_failure}
        (evidence / (label + '-mutation.json')).write_text(json.dumps(record, indent=2) + '\n')
        try:
            path.write_bytes(mutant)
            output = run(label + '-mutant', command, allowed=(1,), timeout=900).read_text()
            assert re.search(r'^--- FAIL: ' + re.escape(test) + r'\b', output, re.MULTILINE), output
            assert expected_failure in output and '[build failed]' not in output, output
            record['outcome'] = 'baseline_passed_compiling_mutant_failed_expected_behavior'
        finally:
            path.write_bytes(original)
            assert path.read_bytes() == original
            record['source_restored'] = True
            (evidence / (label + '-mutation.json')).write_text(json.dumps(record, indent=2) + '\n')
        results.append(record)
    (evidence / 'mutation-results.json').write_text(json.dumps(results, indent=2) + '\n')
elif mode == 'website':
    provenance.update(node=text(['node', '--version']), npm=text(['npm', '--version']))
    (evidence / 'provenance.json').write_text(json.dumps(provenance, indent=2) + '\n')
    run('npm-ci', ['npm', 'ci'], cwd=source / 'website', timeout=900)
    run('website-build', ['npm', 'run', 'build'], cwd=source / 'website', timeout=900)
else:
    raise RuntimeError('unknown verification mode')

status = text(['git', 'status', '--porcelain'])
(evidence / 'final-source-status.json').write_text(json.dumps({'head': head, 'status': status}, indent=2) + '\n')
assert not status, status
