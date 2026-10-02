#!/usr/bin/env python3
import os
from pathlib import Path
import subprocess
import sys
import tempfile

with tempfile.TemporaryDirectory(prefix="go-upgrade-test-buildcache-") as cache:
    env = dict(os.environ, UPGRADE_GO_TEST_GOCACHE=cache)
    for suite in ("batches.py", "retries.py", "shared_cache.py", "concurrency.py", "build_command.py", "output.py"):
        subprocess.run([sys.executable, str(Path(__file__).parent / suite)], env=env, check=True)
