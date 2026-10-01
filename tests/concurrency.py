import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import time

from fixtures import Fixture, REAL_GO, SCRIPT, metadata_queries, version_queries


def maximum_queries(path):
    events = sorted((json.loads(line) for line in path.read_text().splitlines()), key=lambda e: e[1])
    active, maximum, directories = 0, 0, set()
    for event, _, _, directory in events:
        active += 1 if event == "start" else -1
        maximum = max(maximum, active)
        directories.add(directory)
        assert active >= 0
    assert active == 0
    return maximum, directories


with Fixture() as f:
    libraries = [f"example.com/concurrent/lib{i}" for i in range(8)]
    for path in libraries:
        f.publish(path, "v1.0.0", "package lib\nfunc Value() int {return 1}\n")
        f.publish(path, "v1.1.0", "package lib\nfunc Value() int {return 2}\n")

    durations = []
    for workers in (1, 4):
        p = f.project(f"workers-{workers}", libraries)
        timeline = p / "queries.jsonl"
        start = time.monotonic()
        output, calls = f.run(p, extra_env={
            "UPGRADE_GO_WORKERS": str(workers), "QUERY_DELAY": "0.12", "QUERY_TIMELINE": str(timeline),
            "UPGRADE_GO_CACHE_DIR": str(f.root / f"workers-cache-{workers}"),
        })
        durations.append(time.monotonic() - start)
        maximum, directories = maximum_queries(timeline)
        assert maximum == workers, (workers, maximum)
        assert len(directories) == workers and str(p) not in directories, directories
        assert len(version_queries(calls)) == 8 and len(metadata_queries(calls)) == 8
        assert len([a for a in calls if a[0] == "get"]) == 1
    assert durations[1] < durations[0], durations
    print(f"PASS bounded isolated workers; injected 120ms/query: {durations[0]:.2f}s serial -> {durations[1]:.2f}s with four workers", flush=True)

    # Produce literal legacy cache files, using the shell's exact scope calculation.
    # Both reads and writes must remain usable by the existing shell implementation.
    settings = ["GOPROXY", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOSUMDB", "GOINSECURE", "GOAUTH", "GOFLAGS"]
    local = subprocess.check_output([REAL_GO, "env", "GOVERSION"], env=f.base_env, text=True).strip().removeprefix("go")
    config = subprocess.check_output([REAL_GO, "env", "-json", *settings], env=f.base_env, text=True).rstrip("\n")
    scope = hashlib.sha256((local + "\n" + config + "\n").encode()).hexdigest()
    cache_root = f.root / "legacy-format"
    shared = cache_root / "v1" / "1.25.3" / scope
    (shared / "versions").mkdir(parents=True)
    (shared / "metadata").mkdir()
    for path in libraries:
        (shared / "versions" / hashlib.sha256(path.encode()).hexdigest()).write_text(f"{int(time.time())}\nv1.1.0\nv1.1.0\nv1.0.0\n")
        (shared / "metadata" / hashlib.sha256((path + "@v1.1.0").encode()).hexdigest()).write_text("0\n")
    p = f.project("legacy-reader", libraries)
    output, calls = f.run(p, extra_env={"UPGRADE_GO_CACHE_DIR": str(cache_root)})
    assert not version_queries(calls) and not metadata_queries(calls), calls
    print("PASS legacy cache format and byte-identical resolver scope", flush=True)

    legacy = os.environ.get("UPGRADE_GO_LEGACY_SCRIPT")
    if legacy:
        p = f.project("shell-writer", libraries)
        _, calls = f.run(p, script=legacy, extra_env={"UPGRADE_GO_CACHE_DIR": str(f.root / "shell-to-go")})
        assert len(version_queries(calls)) == 8
        p = f.project("go-reader", libraries)
        _, calls = f.run(p, extra_env={"UPGRADE_GO_CACHE_DIR": str(f.root / "shell-to-go")})
        assert not version_queries(calls) and not metadata_queries(calls)
        p = f.project("go-writer", libraries)
        _, calls = f.run(p, extra_env={"UPGRADE_GO_CACHE_DIR": str(f.root / "go-to-shell")})
        assert len(version_queries(calls)) == 8
        p = f.project("shell-reader", libraries)
        _, calls = f.run(p, script=legacy, extra_env={"UPGRADE_GO_CACHE_DIR": str(f.root / "go-to-shell")})
        assert not version_queries(calls) and not metadata_queries(calls)
        print("PASS bidirectional cache sharing with backed-up shell script", flush=True)

    # NDS routing always uses full make builds unless explicitly configured otherwise.
    make = f.wrapper_dir / "make"
    make.write_text('''#!/usr/bin/env python3
import json, os, subprocess, sys
assert sys.argv[1:] == ['clean', 'build']
assert os.environ['GOTOOLCHAIN'] == 'local' and os.environ['GOWORK'] == 'off'
with open(os.environ['MAKE_TRACE'], 'a') as f: f.write(json.dumps(sys.argv[1:])+'\\n')
sys.exit(subprocess.run(['go','build','./...']).returncode)
''')
    make.chmod(0o755)
    nds = f.root / "go-upgrade-nds"
    nds.symlink_to(SCRIPT)
    for custom in (False, True):
        p = f.project(f"nds-{custom}", libraries[:1])
        trace = p / "make.jsonl"
        output, calls = f.run(p, script=str(nds), extra_env={
            "MAKE_TRACE": str(trace), "UPGRADE_GO_CHECK_COMMAND": "go build ./..." if custom else "",
        })
        makes = [json.loads(s) for s in trace.read_text().splitlines()]
        assert len(makes) == (2 if custom else 4), makes
        assert libraries[0] + " v1.1.0" in (p / "go.mod").read_text()
    print("PASS NDS full baseline/final builds and optional intermediate command", flush=True)

    # A too-old local compiler fails before builds, metadata queries, or module changes.
    p = f.project("local-too-old", libraries[:1])
    original = [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
    output, calls = f.run(p, expect=1, extra_env={"FAKE_GOVERSION": "go1.24.0"})
    assert calls == [["env", "GOVERSION"]], calls
    assert original == [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
    print("PASS local compiler too old exits immediately", flush=True)

    # Resolver metadata cannot override a project's replace directive.
    local_library = f.root / "local-library"
    local_library.mkdir()
    (local_library / "go.mod").write_text(f"module {libraries[0]}\n\ngo 1.25.0\n")
    (local_library / "lib.go").write_text("package lib\nfunc Value() int { return 42 }\n")
    p = f.project("replaced", libraries[:1])
    with (p / "go.mod").open("a") as mod:
        mod.write(f"\nreplace {libraries[0]} => {local_library}\n")
    _, calls = f.run(p)
    assert not version_queries(calls) and not metadata_queries(calls)
    assert not any(a[0] == "get" for a in calls)
    print("PASS replaced dependencies are left under project control", flush=True)

    # Cancellation after go get changes the graph must restore exact originals.
    p = f.project("interrupted", libraries[:1])
    originals = [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
    marker = p / "waiting-for-signal"
    env = f.environment(p, {"WAIT_FOR_GET": str(marker)})
    proc = subprocess.Popen([SCRIPT, "1.25.3", "--skip-tests", "--skip-vet", "--upgrade-deps"], cwd=p, env=env,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        deadline = time.monotonic() + 30
        while not marker.exists():
            assert proc.poll() is None, proc.communicate()
            assert time.monotonic() < deadline, "go get never started"
            time.sleep(0.02)
        assert (p / "go.mod").read_bytes() != originals[0]
        # Another invocation in this same directory must fail without touching the graph.
        second = subprocess.run([SCRIPT, "1.25.3", "--skip-tests", "--skip-vet"], cwd=p, env=env,
                                capture_output=True, text=True, timeout=10)
        assert second.returncode == 1 and "another upgrade" in second.stderr, second
        proc.send_signal(signal.SIGTERM)
        stdout, stderr = proc.communicate(timeout=10)
        assert proc.returncode == 143, (proc.returncode, stdout, stderr)
        assert "Restored original go.mod and go.sum." in stderr
        assert originals == [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
    finally:
        if proc.poll() is None:
            proc.send_signal(signal.SIGTERM)
            proc.communicate(timeout=10)
    print("PASS interrupt rollback and rejection of concurrent upgrades in one project", flush=True)
