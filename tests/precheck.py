"""Affected compilation, custom commands, and conservative acceptance checks."""
import json
import subprocess
import sys
from fixtures import Fixture, REAL_GO


with Fixture() as f:
    library = "example.com/precheck/lib"
    f.publish(library, "v1.0.0", "package lib\nfunc Value() int {return 1}\n")
    f.publish(library, "v1.1.0", "package lib\nfunc Value() int {return 1}\n")
    f.publish(library, "v1.2.0", "package lib\nfunc Renamed() int {return 1}\n")
    build = f.root / "custom-build.py"
    build.write_text('''import json, pathlib, subprocess, sys
root = pathlib.Path.cwd()
with (root / 'custom-builds.jsonl').open('a') as f: f.write(json.dumps('build')+'\\n')
generated = root / 'generated.go'
if not generated.exists(): generated.write_text('package main\\nfunc generated() {}\\n')
sys.exit(subprocess.run(['go', 'build', '-o', '/dev/null', './...']).returncode)
''')
    command = f"{sys.executable} {build}"
    counts = []
    for enabled in (False, True):
        p = f.project(f"custom-precheck-{enabled}", [library])
        main = p / "main.go"
        main.write_text(main.read_text().replace("func main() {", "func main() { generated();"))
        independent = p / "independent"
        independent.mkdir()
        (independent / "lib.go").write_text("package independent\nfunc Value() int {return 1}\n")
        flags = ["--build-command", command] + (["--compile-precheck"] if enabled else [])
        output, calls = f.run(p, extra_flags=flags)
        assert f"{library} v1.1.0" in (p / "go.mod").read_text(), output
        counts.append(len((p / "custom-builds.jsonl").read_text().splitlines()))
        prechecks = [a for a in calls if a[:3] == ["build", "-o", "/dev/null"] and a[-1] != "./..."]
        if enabled:
            assert len(prechecks) == 2 and all(a[3:] == ["example.com/app"] for a in prechecks), prechecks
            assert "Compile prechecks: 2 (1 rejected)" in output
            assert "Build validations: 4" in output
        else:
            assert not prechecks, prechecks
        assert not (p / "app").exists(), "prechecks must discard build artifacts"
    assert counts == [5, 4], counts
    print("PASS precheck avoids rejected custom build; generated files reused; unrelated packages excluded", flush=True)

    # Package lookup failures/incomplete data must use the existing validator.
    for mode in ("fail", "incomplete"):
        p = f.project(f"inspection-{mode}", [library])
        output, calls = f.run(p, extra_flags=["--compile-precheck"], extra_env={"PACKAGE_INSPECTION_MODE": mode})
        assert f"{library} v1.1.0" in (p / "go.mod").read_text(), output
        assert "Compile prechecks: 0 (0 rejected)" in output
        assert "Compile precheck unavailable" in output
    print("PASS unavailable or incomplete inspection preserves normal validation", flush=True)

    # A dependency used only by tests needs no production compile precheck.
    p = f.project("test-only-dependency", [], test=f'''package main
import ("testing"; lib "{library}")
func TestValue(t *testing.T) {{ if lib.Value()!=1 {{t.Fatal("changed")}} }}
''')
    result = subprocess.run([REAL_GO, "mod", "edit", f"-require={library}@v1.0.0"], cwd=p, env=f.base_env, capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    result = subprocess.run([REAL_GO, "mod", "tidy"], cwd=p, env=f.base_env, capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    output, calls = f.run(p, extra_flags=["--compile-precheck", "--verbose"])
    assert f"{library} v1.2.0" in (p / "go.mod").read_text(), output
    assert "no affected production packages" in output
    assert "Compile prechecks: 0 (0 rejected)" in output
    print("PASS test-only dependencies respect skip-tests/skip-vet", flush=True)

    behavior = "example.com/precheck/behavior"
    f.publish(behavior, "v1.0.0", "package lib\nfunc Value() int {return 1}\n")
    f.publish(behavior, "v1.1.0", "package lib\nfunc Value() int {return 2}\n")
    p = f.project("precheck-keeps-tests", [behavior], test=f'''package main
import ("testing"; lib "{behavior}")
func TestValue(t *testing.T) {{ if lib.Value()!=1 {{t.Fatal("changed behavior")}} }}
''')
    output, calls = f.run(p, extra_flags=["--compile-precheck"], enabled_tests=True)
    assert f"{behavior} v1.0.0" in (p / "go.mod").read_text(), output
    assert "Compile prechecks: 1 (0 rejected)" in output
    assert any(a[:2] == ["test", "-count=1"] for a in calls)
    assert any(a[:2] == ["test", "-race"] for a in calls)
    print("PASS passing prechecks retain full enabled tests/race checks", flush=True)

    # Custom build tags must be present in the inherited Go environment too.
    p = f.project("precheck-build-tags", [behavior])
    (p / "tagged.go").write_text("//go:build custom\n\npackage main\nfunc tagged() {}\n")
    main = p / "main.go"
    main.write_text(main.read_text().replace("func main() {", "func main() { tagged();"))
    output, calls = f.run(p, extra_flags=["--compile-precheck", "--build-command=go build -o /dev/null -tags=custom ./..."],
                          extra_env={"GOFLAGS": "-buildvcs=false -tags=custom"})
    assert f"{behavior} v1.1.0" in (p / "go.mod").read_text(), output
    assert "Compile prechecks: 1 (0 rejected)" in output
    print("PASS matching inherited build tags apply to prechecks", flush=True)

    # No compiler attribution: relationship grouping still coordinates modules
    # whose newest versions must be selected together, across namespaces.
    api, client, bad = "z.example/api", "a.example/client", "b.example/bad"
    f.publish(api, "v1.0.0", "package lib\nfunc Value() int {return 1}\n")
    f.publish(api, "v1.1.0", "package lib\nfunc Value() int {return 1}\nfunc NewValue() int {return 1}\n")
    f.publish(client, "v1.0.0", f'package lib\nimport api "{api}"\nfunc Value() int {{return api.Value()}}\n', requires={api: "v1.0.0"})
    f.publish(client, "v1.1.0", f'package lib\nimport api "{api}"\nfunc Value() int {{return api.NewValue()}}\n', requires={api: "v1.0.0"})
    for version in ("v1.0.0", "v1.1.0"):
        f.publish(bad, version, "package lib\nfunc Value() int {return 1}\n")
    opaque = f.root / "opaque-build.py"
    opaque.write_text(f'''import pathlib, subprocess, sys
if '{bad} v1.1.0' in pathlib.Path('go.mod').read_text():
    print('custom policy rejected this graph', file=sys.stderr)
    sys.exit(9)
sys.exit(subprocess.run(['go', 'build', './...']).returncode)
''')
    p = f.project("opaque-failure-grouping", [client, bad, api])
    output, calls = f.run(p, extra_flags=["--build-command", f"{sys.executable} {opaque}"])
    assert f"{api} v1.1.0" in (p / "go.mod").read_text(), output
    assert f"{client} v1.1.0" in (p / "go.mod").read_text(), output
    assert f"{bad} v1.0.0" in (p / "go.mod").read_text(), output
    assert any(a[0] == "get" and f"{api}@v1.1.0" in a and f"{client}@v1.1.0" in a and f"{bad}@v1.1.0" not in a for a in calls), calls
    assert "dependency groups" in output, output
    assert "custom policy rejected this graph" in (p / "output.log").read_text()
    print("PASS opaque custom failures use actual cross-namespace dependency grouping", flush=True)
