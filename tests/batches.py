import collections
import hashlib
import json
import os
import pathlib
import subprocess
from fixtures import Fixture, SCRIPT

with Fixture() as fixture:
    root, base_env, wrapper_dir = fixture.root, fixture.base_env, fixture.wrapper_dir
    publish, project, run = fixture.publish, fixture.project, fixture.run
    # Most dependencies succeed; only one requires source-sensitive fallback.
    libs = [f'example.com/m{i:02}' for i in range(16)]
    for path in libs:
        publish(path, 'v1.0.0', 'package lib\nfunc Value() int {return 1}\n')
        publish(path, 'v1.1.0', 'package lib\nfunc Value() int {return 2}\n')
    bad = libs[-1]
    publish(bad, 'v1.2.0', 'package lib\nfunc Renamed() int {return 3}\n')
    publish(bad, 'v1.3.0', 'package lib\nfunc Value() int {return 4}\n', go='1.27.0')
    p = project('split-fallback', libs)
    output, calls = run(p)
    assert 'Splitting into' in output
    assert f'Accepted {bad}@v1.1.0' in output
    mod = (p/'go.mod').read_text()
    assert all(f'{path} v1.1.0' in mod for path in libs), mod
    queries = collections.Counter(a[-1] for a in calls if a[:3] == ['list','-m','-versions'])
    assert len(queries) == 16 and set(queries.values()) == {1}, queries
    metadata = collections.Counter(a[-1] for a in calls if a[:3] == ['list','-m','-f'] and a[3] == '{{.GoMod}}')
    assert set(metadata.values()) == {1}, metadata
    assert not any(a[0]=='get' and f'{bad}@v1.3.0' in a for a in calls)
    assert sum(a[0]=='get' and a[1:2]==[f'{bad}@v1.2.0'] for a in calls) == 1
    new_builds = sum(a==['build','./...'] for a in calls)
    print('PASS split/fallback, metadata cache, and no unrelated retries:', new_builds, 'build calls', flush=True)

    all_good = project('all-good', libs[:8])
    output, calls = run(all_good)
    gets = [a for a in calls if a[0]=='get']
    assert len(gets)==1 and len([a for a in gets[0] if '@v' in a])==8, gets
    assert 'Accepted batch (' in output
    print('PASS all-compatible upgrades in one batch', flush=True)

    # A transitive upgrade from the first sibling makes the second unnecessary.
    api='example.com/related/api'; client='example.com/related/client'
    publish(api,'v1.0.0','package lib\nfunc Value() int {return 1}\n')
    publish(api,'v1.1.0','package lib\nfunc Value() int {return 2}\n')
    publish(client,'v1.0.0',f'package lib\nimport api "{api}"\nfunc Value() int {{return api.Value()}}\n',requires={api:'v1.0.0'})
    publish(client,'v1.1.0',f'package lib\nimport api "{api}"\nfunc Value() int {{return api.Value()}}\n',requires={api:'v1.1.0'})
    grouped = project('related', [api, client, bad])
    output, calls = run(grouped)
    assert any(a[0]=='get' and f'{api}@v1.1.0' in a and f'{client}@v1.1.0' in a and f'{bad}@v1.2.0' not in a for a in calls), calls
    assert f'{client} v1.1.0' in (grouped/'go.mod').read_text()
    print('PASS related modules stay together when splitting', flush=True)

    child='example.com/new/child'; parent='example.com/new/parent'
    publish(child,'v1.0.0','package lib\nfunc Value() int {return 1}\n')
    publish(child,'v1.1.0','package lib\nfunc Value() int {return 2}\n')
    publish(parent,'v1.0.0','package lib\nfunc Value() int {return 1}\n')
    publish(parent,'v1.1.0',f'package lib\nimport child "{child}"\nfunc Value() int {{return child.Value()}}\n',requires={child:'v1.0.0'})
    introduced = project('introduced',[parent])
    output,calls = run(introduced)
    assert f'{child} v1.1.0' in (introduced/'go.mod').read_text()
    assert sum(a[:3]==['list','-m','-versions'] and a[-1]==parent for a in calls)==1
    assert sum(a[:3]==['list','-m','-versions'] and a[-1]==child for a in calls)==1
    print('PASS newly introduced dependencies get their own discovery pass', flush=True)

    default = project('default', libs[:2])
    output,calls = run(default,upgrade=False)
    assert not any(a[0]=='get' for a in calls)
    assert all(f'{path} v1.0.0' in (default/'go.mod').read_text() for path in libs[:2])
    print('PASS default mode keeps dependency versions', flush=True)

    tested = project('test-failure',[libs[0]],test=f'package main\nimport ("testing"; lib "{libs[0]}")\nfunc TestValue(t *testing.T) {{if lib.Value()!=1 {{t.Fatal("changed behavior")}}}}\n')
    output,calls = run(tested,enabled_tests=True)
    assert f'{libs[0]} v1.0.0' in (tested/'go.mod').read_text()
    assert 'Retained' in output
    print('PASS enabled tests reject a build-compatible behavior change', flush=True)

    broken = project('rollback',[],broken=True)
    original=(broken/'go.mod').read_bytes()
    output,calls = run(broken,expect=1)
    assert (broken/'go.mod').read_bytes()==original and not (broken/'go.sum').exists()
    print('PASS failed baseline restores original module files', flush=True)
    empty = project('empty',[])
    output,calls = run(empty)
    assert not any(a[0]=='get' for a in calls)
    print('PASS module without dependencies', flush=True)
