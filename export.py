import hashlib
import json
import os
from pathlib import Path
import re
import sys

source = Path(sys.argv[1]).resolve()
destination = Path(sys.argv[2]).resolve()
destination.mkdir(parents=True, exist_ok=True, mode=0o755)
secrets = [value.encode() for name, value in os.environ.items()
           if re.search(r'TOKEN|SECRET|PASSWORD|API_KEY|PRIVATE_KEY', name)
           and len(value) >= 8]
manifest = {'source_exists': source.exists(), 'files': []}
for path in sorted(source.rglob('*')):
    if path.is_symlink() or not path.is_file():
        continue
    relative = path.relative_to(source)
    if 'tools' in relative.parts or path.name in ('gitmoot', 'cli-smoke', 'worker-smoke'):
        continue
    data = path.read_bytes()
    data.decode('utf-8')
    original_hash = hashlib.sha256(data).hexdigest()
    changed = False
    for secret in secrets:
        if secret in data:
            data = data.replace(secret, b'[REDACTED_RUNTIME_SECRET]')
            changed = True
    target = destination / relative
    target.parent.mkdir(parents=True, exist_ok=True, mode=0o755)
    target.write_bytes(data)
    target.chmod(0o644)
    assert target.read_bytes() == data
    manifest['files'].append({'path': str(relative), 'redacted': changed,
                              'original_sha256': original_hash,
                              'exported_sha256': hashlib.sha256(data).hexdigest()})
(destination / 'export-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
print(json.dumps({'exported_files': len(manifest['files']),
                  'redacted_files': sum(row['redacted'] for row in manifest['files'])}))
