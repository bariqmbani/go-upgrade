"""Real Go modules served from a local file proxy; no network is needed."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import zipfile

SCRIPT = os.environ.get("UPGRADE_GO_TEST_BINARY", str(Path(__file__).resolve().parents[1] / "bin/go-upgrade"))
REAL_GO = shutil.which("go")

WRAPPER = '''#!/usr/bin/env python3
import json, os, subprocess, sys, time
assert os.environ.get('GOTOOLCHAIN') == 'local'
assert os.environ.get('GOWORK') == 'off'
args = sys.argv[1:]
def append(path, entry):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    try: os.write(fd, (json.dumps(entry)+'\\n').encode())
    finally: os.close(fd)
append(os.environ['GO_TRACE'], args)
if args == ['env', 'GOVERSION'] and os.environ.get('FAKE_GOVERSION'):
    print(os.environ['FAKE_GOVERSION'])
    sys.exit(0)
metadata = args[:4] == ['list','-m','-f','{{.GoMod}}']
versions = args[:3] == ['list','-m','-versions']
query = metadata or versions or (args[:4] == ['list','-m','-f','{{.Version}}'])
timeline = os.environ.get('QUERY_TIMELINE')
if query and timeline:
    append(timeline, ['start', time.monotonic_ns(), os.getpid(), os.getcwd()])
if query: time.sleep(float(os.environ.get('QUERY_DELAY', '0')))
if args[:2] == ['list', '-deps'] and os.environ.get('PACKAGE_INSPECTION_MODE'):
    if os.environ['PACKAGE_INSPECTION_MODE'] == 'incomplete':
        print(json.dumps({'ImportPath': 'example.com/app', 'Name': 'main',
                         'Module': {'Path': 'example.com/app', 'Main': True},
                         'Incomplete': True, 'Error': {'Err': 'injected incomplete package inspection'}}))
        status = 0
    else:
        print('injected package inspection failure', file=sys.stderr)
        status = 1
elif metadata and os.environ.get('FAIL_METADATA') == 'true':
    print('temporary metadata lookup failure', file=sys.stderr)
    if os.environ.get('LONG_DIAGNOSTICS'):
        print('METADATA-FIRST', file=sys.stderr)
        for i in range(8): print('metadata detail', i, file=sys.stderr)
        print('METADATA-LAST', file=sys.stderr)
    status = 1
elif versions and os.environ.get('FAIL_VERSIONS') == 'true':
    print('temporary version lookup failure', file=sys.stderr)
    if os.environ.get('LONG_DIAGNOSTICS'):
        print('VERSIONS-FIRST', file=sys.stderr)
        for i in range(8): print('version detail', i, file=sys.stderr)
        print('VERSIONS-LAST', file=sys.stderr)
    status = 1
elif args[:4] == ['list','-m','-f','{{.Version}}'] and os.environ.get('FAIL_LATEST') == 'true':
    print('LATEST-FIRST', file=sys.stderr)
    for i in range(8): print('latest detail', i, file=sys.stderr)
    print('LATEST-LAST', file=sys.stderr)
    status = 1
else:
    status = subprocess.run([os.environ['REAL_GO'], *args]).returncode
if args[:1] == ['get'] and status == 0 and os.environ.get('WAIT_FOR_GET'):
    with open(os.environ['WAIT_FOR_GET'], 'w') as f: f.write('ready')
    time.sleep(60)
if args[:3] == ['build','-o','/dev/null'] and args[-1] != './...' and status == 0 and os.environ.get('WAIT_FOR_PRECHECK'):
    with open(os.environ['WAIT_FOR_PRECHECK'], 'w') as f: f.write('ready')
    time.sleep(60)
if query and timeline:
    append(timeline, ['end', time.monotonic_ns(), os.getpid(), os.getcwd()])
sys.exit(status)
'''


class Fixture:
    def __enter__(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="go-upgrade-tests-")
        self.root = Path(self.temporary.name)
        self.proxy = self.root / "proxy"
        self.proxy.mkdir()
        self.wrapper_dir = self.root / "bin"
        self.wrapper_dir.mkdir()
        wrapper = self.wrapper_dir / "go"
        wrapper.write_text(WRAPPER)
        wrapper.chmod(0o755)
        self.base_env = dict(os.environ, GOPROXY=self.proxy.as_uri(), GOSUMDB="off", GOPRIVATE="",
                             GONOPROXY="", GONOSUMDB="", GOFLAGS="-buildvcs=false", GOTOOLCHAIN="local",
                             GOWORK="off", GOCACHE=os.environ.get("UPGRADE_GO_TEST_GOCACHE", str(self.root / "buildcache")),
                             GOPATH=str(self.root / "gopath"), GOMODCACHE=str(self.root / "modcache"),
                             REAL_GO=REAL_GO, UPGRADE_GO_CACHE_DIR=str(self.root / "shared-cache"),
                             UPGRADE_GO_WORKERS="4", UPGRADE_GO_CACHE_TTL="86400", UPGRADE_GO_CHECK_COMMAND="")
        return self

    def __exit__(self, *args):
        self.temporary.cleanup()

    def publish(self, path, version, source, go="1.25.0", requires=None):
        dest = self.proxy / path / "@v"
        dest.mkdir(parents=True, exist_ok=True)
        mod = f"module {path}\n\ngo {go}\n"
        if requires:
            mod += "\nrequire (\n" + "".join(f" {p} {v}\n" for p, v in requires.items()) + ")\n"
        (dest / (version + ".mod")).write_text(mod)
        (dest / (version + ".info")).write_text(json.dumps({"Version": version, "Time": "2026-01-01T00:00:00Z"}))
        with zipfile.ZipFile(dest / (version + ".zip"), "w") as z:
            z.writestr(f"{path}@{version}/go.mod", mod)
            z.writestr(f"{path}@{version}/lib.go", source)
        (dest / "list").write_text("\n".join(sorted(p.stem for p in dest.glob("*.info"))) + "\n")

    def project(self, case, dependencies, test=None, broken=False):
        p = self.root / case
        p.mkdir()
        mod = "module example.com/app\n\ngo 1.25.0\n"
        if dependencies:
            mod += "\nrequire (\n" + "".join(f" {d} v1.0.0\n" for d in dependencies) + ")\n"
        (p / "go.mod").write_text(mod)
        imports = "".join(f' m{i} "{d}"\n' for i, d in enumerate(dependencies))
        calls = "; ".join(f"println(m{i}.Value())" for i in range(len(dependencies)))
        if broken:
            calls = "missing()"
        source = "package main\n"
        if imports:
            source += "import (\n" + imports + ")\n"
        source += "func main() { " + calls + " }\n"
        (p / "main.go").write_text(source)
        if test:
            (p / "main_test.go").write_text(test)
        if not broken:
            setup = subprocess.run([REAL_GO, "mod", "tidy"], cwd=p, env=self.base_env, capture_output=True, text=True)
            assert setup.returncode == 0, setup.stderr
        return p

    def environment(self, project, extra=None):
        trace = project / "trace.jsonl"
        trace.write_text("")
        env = dict(self.base_env, GO_TRACE=str(trace), PATH=str(self.wrapper_dir) + ":" + os.environ["PATH"])
        env.update(extra or {})
        return env

    def run(self, p, script=SCRIPT, enabled_tests=False, upgrade=True, expect=0, target="1.25.3", refresh=False, extra_env=None, extra_flags=None):
        env = self.environment(p, extra_env)
        flags = ["--skip-vet"] + ([] if enabled_tests else ["--skip-tests"]) + (["--upgrade-deps"] if upgrade else [])
        if refresh:
            flags.append("--refresh-cache")
        flags.extend(extra_flags or [])
        result = subprocess.run([script, target, *flags], cwd=p, env=env, capture_output=True, text=True, timeout=180)
        (p / "output.log").write_text(result.stdout + result.stderr)
        assert result.returncode == expect, (result.returncode, result.stdout, result.stderr)
        calls = [json.loads(s) for s in Path(env["GO_TRACE"]).read_text().splitlines()]
        return result.stdout, calls


def version_queries(calls):
    return [a for a in calls if a[:3] == ["list", "-m", "-versions"]]


def metadata_queries(calls):
    return [a for a in calls if a[:4] == ["list", "-m", "-f", "{{.GoMod}}"]]
