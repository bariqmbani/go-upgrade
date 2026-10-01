import json
from fixtures import Fixture, metadata_queries, version_queries


with Fixture() as f:
    library = "example.com/build/lib"
    for version in ("v1.0.0", "v1.1.0"):
        f.publish(library, version, "package lib\nfunc Value() int {return 1}\n")

    make = f.wrapper_dir / "make"
    make.write_text('''#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys
assert sys.argv[1:] == ['clean', 'build']
assert os.environ['GOTOOLCHAIN'] == 'local' and os.environ['GOWORK'] == 'off'
trace = pathlib.Path(os.environ['MAKE_TRACE'])
with trace.open('a') as f: f.write(json.dumps([sys.argv[1:], os.getcwd()])+'\\n')
count = len(trace.read_text().splitlines())
if count == int(os.environ.get('FAIL_MAKE_AT', '0')):
    sys.exit(42)
if os.environ.get('REJECT_UPGRADE') == 'true' and ' v1.1.0' in pathlib.Path('go.mod').read_text():
    sys.exit(5)
sys.exit(subprocess.run(['go','build','./...']).returncode)
''')
    make.chmod(0o755)

    # Without the flag, the default Go build never invokes make.
    p = f.project("default-build", [library])
    trace = p / "make.jsonl"
    _, calls = f.run(p, extra_env={"MAKE_TRACE": str(trace)})
    assert not trace.exists() and sum(a == ["build", "./..."] for a in calls) == 4
    print("PASS default build remains go build ./...", flush=True)

    # Both flag forms use the supplied command in every build phase.
    for equals in (False, True):
        p = f.project(f"custom-build-{equals}", [library])
        trace = p / "make.jsonl"
        flags = ["--build-command=make clean build"] if equals else ["--build-command", "make clean build"]
        output, calls = f.run(p, extra_flags=flags, extra_env={"MAKE_TRACE": str(trace)})
        builds = [json.loads(s) for s in trace.read_text().splitlines()]
        assert len(builds) == 4 and all(cwd == str(p) for _, cwd in builds), builds
        assert library + " v1.1.0" in (p / "go.mod").read_text()
        assert "Build command: make clean build" in output
        assert not version_queries(calls) and not metadata_queries(calls), calls
    print("PASS custom command with both flag forms validates baseline, candidates, and final build", flush=True)

    p = f.project("custom-intermediate", [library])
    trace = p / "make.jsonl"
    f.run(p, extra_flags=["--build-command=make clean build"], extra_env={
        "MAKE_TRACE": str(trace), "UPGRADE_GO_CHECK_COMMAND": "go build ./...",
    })
    assert len(trace.read_text().splitlines()) == 2
    print("PASS intermediate override keeps the selected baseline and final build command", flush=True)

    p = f.project("shell-quoting", [])
    command = '''test "$BUILD_LABEL" = 'spaces in label' && printf '%s\\n' "$BUILD_LABEL" > build-marker && go build ./...'''
    f.run(p, upgrade=False, extra_flags=["--build-command", command], extra_env={"BUILD_LABEL": "spaces in label"})
    assert (p / "build-marker").read_text() == "spaces in label\n"
    print("PASS shell quoting, environment, chaining, and module-root working directory", flush=True)

    # A custom build can reject an otherwise compilable dependency update.
    p = f.project("custom-rejection", [library])
    output, _ = f.run(p, extra_flags=["--build-command=make clean build"], extra_env={
        "MAKE_TRACE": str(p / "make.jsonl"), "REJECT_UPGRADE": "true",
    })
    assert library + " v1.0.0" in (p / "go.mod").read_text() and "Retained" in output
    print("PASS custom build failures reject incompatible candidates", flush=True)

    # Baseline and final failures restore exact originals and propagate exit codes.
    p = f.project("baseline-failure", [library])
    original = [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
    _, calls = f.run(p, expect=7, extra_flags=["--build-command", "go mod edit -go=1.26.0; exit 7"])
    assert original == [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
    assert not any(a[0] == "get" for a in calls)

    p = f.project("final-failure", [library])
    original = [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
    _, calls = f.run(p, expect=42, extra_flags=["--build-command=make clean build"], extra_env={
        "MAKE_TRACE": str(p / "make.jsonl"), "FAIL_MAKE_AT": "4",
    })
    assert any(a[0] == "get" for a in calls)
    assert original == [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
    print("PASS baseline/final failure rollback and custom command exit codes", flush=True)
