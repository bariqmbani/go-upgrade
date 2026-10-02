"""Dependency grouping and conservative fallback when retry hints are unavailable."""
import sys
from fixtures import Fixture

with Fixture() as f:
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

    for mode in ("fail", "incomplete"):
        p = f.project(f"retry-inspection-{mode}", [client, bad, api])
        output, calls = f.run(p, extra_flags=["--build-command", f"{sys.executable} {opaque}"],
                              extra_env={"PACKAGE_INSPECTION_MODE": mode})
        assert f"{api} v1.1.0" in (p / "go.mod").read_text(), output
        assert f"{bad} v1.0.0" in (p / "go.mod").read_text(), output
        assert "Splitting into" in output
        assert not any(a[:3] == ["build", "-o", "/dev/null"] for a in calls), calls
    print("PASS unavailable retry inspection falls back to validated smaller batches", flush=True)
