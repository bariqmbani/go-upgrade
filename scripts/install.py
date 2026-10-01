#!/usr/bin/env python3
"""Install all command names atomically, preserving existing commands first."""
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import shutil
import tempfile

project = Path(__file__).resolve().parent.parent
destination = Path(os.environ.get("UPGRADE_GO_INSTALL_DIR", str(Path.home() / ".local/bin"))).expanduser()
destination.mkdir(parents=True, exist_ok=True)
names = ("go-upgrade", "go-upgrade-nds", "upgrade-go", "upgrade-go-nds")
existing = [name for name in names if (destination / name).exists()]
if existing:
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    backup = project / "backups" / stamp
    backup.mkdir(parents=True)
    manifest = {}
    for name in existing:
        source = destination / name
        shutil.copy2(source, backup / name)
        manifest[name] = {"source": str(source), "sha256": hashlib.sha256(source.read_bytes()).hexdigest()}
    (backup / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"Backed up existing commands: {backup}")

fd, temporary = tempfile.mkstemp(prefix=".go-upgrade-", dir=destination)
os.close(fd)
try:
    shutil.copy2(project / "bin/go-upgrade", temporary)
    os.chmod(temporary, 0o755)
    os.replace(temporary, destination / "go-upgrade")
finally:
    if os.path.lexists(temporary):
        os.unlink(temporary)
for name in names[1:]:
    fd, temporary = tempfile.mkstemp(prefix=".go-upgrade-link-", dir=destination)
    os.close(fd)
    os.unlink(temporary)
    try:
        os.symlink("go-upgrade", temporary)
        os.replace(temporary, destination / name)
    finally:
        if os.path.lexists(temporary):
            os.unlink(temporary)
print(f"Installed {', '.join(names)} in {destination}")
