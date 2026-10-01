"""Presentation checks using both pipes and actual Unix pseudo terminals."""
import errno
import fcntl
import json
import os
import pty
import re
import select
import signal
import struct
import subprocess
import termios
import time

from fixtures import Fixture, SCRIPT, metadata_queries, version_queries


def terminal_run(f, project, flags=(), extra=None, width=100, height=24, interrupt=None, resize=False):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))
    env = f.environment(project, {"TERM": "xterm-256color", "CI": "", "NO_COLOR": "1", **(extra or {})})
    proc = subprocess.Popen([SCRIPT, "1.25.3", "--skip-tests", "--skip-vet", "--upgrade-deps", *flags],
                            cwd=project, env=env, stdin=subprocess.DEVNULL, stdout=slave, stderr=slave)
    os.close(slave)
    output = bytearray()
    saw_live_candidate = False
    changed_size = False
    sent_signal = False
    deadline = time.monotonic() + 120
    try:
        while True:
            assert time.monotonic() < deadline, "terminal command timed out"
            ready, _, _ = select.select([master], [], [], 0.1)
            if not ready:
                continue
            try:
                data = os.read(master, 65536)
            except OSError as err:
                if err.errno == errno.EIO:
                    break
                raise
            if not data:
                break
            output.extend(data)
            text = output.decode(errors="replace")
            if "@v1.1.0 | Checking" in text and not saw_live_candidate:
                saw_live_candidate = True
                # This status must be displayed while the query is still running.
                timeline = project / "queries.jsonl"
                if timeline.exists():
                    events = [json.loads(line) for line in timeline.read_text().splitlines()]
                    # The versions and latest queries precede metadata. The
                    # renderer may run before the metadata process even starts.
                    assert sum(e[0] == "end" for e in events) < 3, events
                if interrupt:
                    proc.send_signal(interrupt)
                    sent_signal = True
                if resize:
                    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", 16, 72, 0, 0))
                    changed_size = True
        proc.wait(timeout=10)
        return proc.returncode, output.decode(errors="replace"), saw_live_candidate, changed_size, sent_signal
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=10)
        os.close(master)


with Fixture() as f:
    library = "example.com/presentation/lib"
    f.publish(library, "v1.0.0", "package lib\nfunc Value() int {return 1}\n")
    f.publish(library, "v1.1.0", "package lib\nfunc Value() int {return 2}\n")
    build = "printf 'SUCCESS-MARKER\\n'; go build ./..."

    p = f.project("plain", [library])
    output, _ = f.run(p, extra_flags=["--build-command", build])
    assert "SUCCESS-MARKER\n" not in output.split("Baseline checks...")[1], output
    assert "\x1b" not in output and "\r" not in output, repr(output)
    assert "Upgrade complete" in output and "selected 1 upgrade" in output, output
    assert "Baseline checks 1/1 (100%)" in output and "Final validation 4/4 (100%)" in output, output
    assert "Metadata 1/1 (100%)" in output and "Upgrade complete: 100%" in output, output
    print("PASS concise redirected output without terminal controls or successful command logs", flush=True)

    p = f.project("verbose-cache", [library])
    output, calls = f.run(p, extra_flags=["--verbose", "--build-command", build])
    assert "Cached (shared)" in output and "SUCCESS-MARKER\n" in output, output
    assert not metadata_queries(calls) and not version_queries(calls), calls
    p = f.project("refresh", [library])
    output, _ = f.run(p, refresh=True, extra_flags=["--verbose"])
    assert "Refreshing" in output, output
    print("PASS verbose command transcripts and shared/refresh cache labels", flush=True)

    # Every rejected attempt retains its entire build output, even without verbose.
    p = f.project("rejected", [library])
    command = "if grep -q 'v1.1.0' go.mod; then printf 'BUILD-FIRST\\n'; seq 1 8; printf 'BUILD-LAST\\n' >&2; exit 9; fi; go build ./..."
    output, _ = f.run(p, extra_flags=["--build-command", command])
    diagnostics = (p / "output.log").read_text()
    assert "BUILD-FIRST\n1\n2\n3\n4\n5\n6\n7\n8\nBUILD-LAST\n" in diagnostics, diagnostics
    assert "Candidates:" in diagnostics and "Exit code : 9" in diagnostics, diagnostics
    assert "Retained" in output and library + " v1.0.0" in (p / "go.mod").read_text()
    p = f.project("verbose-fallback", [library])
    output, _ = f.run(p, extra_flags=["--verbose", "--build-command", command])
    assert "Cached (this run)" in output, output
    print("PASS rejected dependency attempts always print full diagnostics and candidate context", flush=True)

    for kind in ("METADATA", "VERSIONS", "LATEST"):
        p = f.project("lookup-error-" + kind.lower(), [library])
        f.run(p, extra_env={"UPGRADE_GO_CACHE_DIR": str(f.root / (kind + "-cache")),
                            "FAIL_" + kind: "true", "LONG_DIAGNOSTICS": "true"})
        diagnostics = (p / "output.log").read_text()
        assert kind + "-FIRST" in diagnostics and kind + "-LAST" in diagnostics, diagnostics
        assert "Exit code: 1" in diagnostics and "exit status 1" in diagnostics, diagnostics
    print("PASS full metadata, version-list, and latest-version errors without --verbose", flush=True)

    for step, fail_at, code in (("baseline", 1, 7), ("final", 4, 42)):
        p = f.project(step + "-error", [library])
        originals = [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
        command = ("n=$(cat build-count 2>/dev/null || echo 0); n=$((n+1)); echo $n > build-count; "
                   f"if [ $n = {fail_at} ]; then printf 'FATAL-FIRST\\n'; seq 1 8; printf 'FATAL-LAST\\n' >&2; exit {code}; fi; go build ./...")
        f.run(p, expect=code, extra_flags=["--build-command", command])
        diagnostics = (p / "output.log").read_text()
        assert "FATAL-FIRST\n1\n2\n3\n4\n5\n6\n7\n8\nFATAL-LAST\n" in diagnostics, diagnostics
        assert f"Exit code : {code}" in diagnostics and "Restored original go.mod and go.sum." in diagnostics
        assert originals == [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
    print("PASS complete baseline/final failures with exact rollback and exit codes", flush=True)

    p = f.project("live", [library])
    code, output, live, resized, _ = terminal_run(f, p, extra={
        "QUERY_DELAY": "0.4", "QUERY_TIMELINE": str(p / "queries.jsonl"),
        "UPGRADE_GO_CACHE_DIR": str(f.root / "live-cache"),
    }, resize=True)
    assert code == 0 and live and resized, (code, repr(output))
    assert re.search(r"\x1b\[23;1HOverall \[[#-]+\] ~\d+% \| Metadata 0/1 \(0%\)", output), repr(output)
    assert "\x1b[24;1H" in output and "\x1b[22;1H\x1b[2K" in output, repr(output)
    assert "\x1b[15;1HOverall" in output and "\x1b[16;1H" in output, repr(output)
    estimates = [int(n) for n in re.findall(r"Overall \[[#-]+\] ~(\d+)%", output)]
    assert estimates and estimates == sorted(estimates) and max(estimates) <= 99, estimates
    assert "\x1b[2K" in output and "\x1b[?25" not in output and "\x1b[32m" not in output, repr(output)
    assert "Upgrade complete" in output and output.endswith("\n"), repr(output)
    print("PASS fixed bottom footer, blank margin, percentages, live checks, resize, and clean completion", flush=True)

    # Real terminals still use plain output for CI, TERM=dumb, or tiny widths.
    for label, extra, width, height in (("ci", {"CI": "true"}, 100, 24), ("dumb", {"TERM": "dumb"}, 100, 24), ("narrow", {}, 40, 24), ("short", {}, 100, 7)):
        p = f.project("terminal-" + label, [library])
        code, output, _, _, _ = terminal_run(f, p, extra=extra, width=width, height=height)
        assert code == 0 and "\x1b" not in output, (code, repr(output))
    print("PASS plain terminal fallbacks for CI, dumb terminals, and narrow widths", flush=True)

    # Both stdout and stderr go to the same real terminal. Long diagnostics must
    # remain complete while the presenter clears and removes its footer.
    p = f.project("terminal-long-error", [library])
    originals = [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
    command = "sleep 0.3; printf 'TERMINAL-FIRST\\n'; seq 1 128; printf 'TERMINAL-LAST\\n' >&2; exit 7"
    code, output, _, _, _ = terminal_run(f, p, flags=["--build-command", command])
    assert code == 7 and "\x1b[23;1HOverall" in output, (code, repr(output))
    expected = "TERMINAL-FIRST\n" + "".join(f"{n}\n" for n in range(1, 129)) + "TERMINAL-LAST\n"
    assert expected in output.replace("\r\n", "\n"), repr(output)
    assert "Upgrade complete: 100%" not in output and "Go upgrade failed" in output, repr(output)
    assert originals == [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
    print("PASS complete long stderr/stdout diagnostics with footer cleanup and rollback", flush=True)

    for sig, expected in ((signal.SIGINT, 130), (signal.SIGTERM, 143)):
        p = f.project("terminal-interrupt-" + str(expected), [library])
        originals = [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
        code, output, live, _, sent = terminal_run(f, p, interrupt=sig, extra={
            "QUERY_DELAY": "0.4", "UPGRADE_GO_CACHE_DIR": str(f.root / ("signal-cache-" + str(expected))),
        })
        assert code == expected and live and sent, (code, repr(output))
        assert "Restored original go.mod and go.sum." in output and "Go upgrade failed" in output, repr(output)
        assert originals == [(p / name).read_bytes() for name in ("go.mod", "go.sum")]
        assert output.endswith("\n") and "\x1b[?25" not in output, repr(output)
    print("PASS SIGINT/SIGTERM clear terminal progress, restore originals, and preserve exit codes", flush=True)
