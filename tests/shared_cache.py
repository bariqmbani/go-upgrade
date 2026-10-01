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
    libs=[f'example.com/shared/lib{i}' for i in range(4)]
    for path in libs:
        publish(path, 'v1.0.0', 'package lib\nfunc Value() int {return 1}\n')
        publish(path, 'v1.1.0', 'package lib\nfunc Value() int {return 2}\n')

    def version_queries(calls):
        return [a for a in calls if a[:3] == ['list','-m','-versions']]

    def metadata_queries(calls):
        return [a for a in calls if a[:3] == ['list','-m','-f'] and a[3] == '{{.GoMod}}']

    first=project('project-a',libs)
    output,calls=run(first)
    assert len(version_queries(calls))==4 and len(metadata_queries(calls))==4, calls
    second=project('project-b',libs)
    output,calls=run(second)
    assert not version_queries(calls) and not metadata_queries(calls), calls
    assert 'Cache hits    : 4 version lists, 4 metadata checks' in output, output
    assert len([a for a in calls if a[0]=='get'])==1
    print('PASS project A -> B: zero repeated version/metadata queries, one upgrade batch',flush=True)

    # A new release is discovered through expiry or explicit refresh.
    publish(libs[0], 'v1.2.0', 'package lib\nfunc Value() int {return 3}\n')
    fresh=project('cache-still-fresh',libs)
    output,calls=run(fresh)
    assert libs[0]+' v1.1.0' in (fresh/'go.mod').read_text()
    assert not version_queries(calls)
    forced=project('refresh',libs)
    output,calls=run(forced,refresh=True)
    assert libs[0]+' v1.2.0' in (forced/'go.mod').read_text()
    assert len(version_queries(calls))==4 and len(metadata_queries(calls))==4
    print('PASS --refresh-cache discovers newer releases',flush=True)

    shared=root/'shared-cache'
    for entry in shared.glob('v1/1.25.3/*/versions/*'):
        lines=entry.read_text().splitlines(); lines[0]='0'; entry.write_text('\n'.join(lines)+'\n')
    expired=project('expired',libs)
    output,calls=run(expired)
    assert len(version_queries(calls))==4 and not metadata_queries(calls), calls
    zero=project('ttl-zero',libs)
    output,calls=run(zero,extra_env={'UPGRADE_GO_CACHE_TTL':'0'})
    assert len(version_queries(calls))==4 and not metadata_queries(calls)
    print('PASS expired/zero TTL refreshes lists while reusing immutable metadata',flush=True)

    # Corrupt entries must be treated as misses.
    scopes=list(shared.glob('v1/1.25.3/*')); assert len(scopes)==1,scopes
    scope=scopes[0]
    (scope/'versions'/hashlib.sha256(libs[0].encode()).hexdigest()).write_text('invalid\n')
    (scope/'metadata'/hashlib.sha256((libs[0]+'@v1.2.0').encode()).hexdigest()).write_text('invalid\n')
    corrupt=project('corrupt',libs)
    output,calls=run(corrupt)
    assert len(version_queries(calls))==1 and len(metadata_queries(calls))==1,calls
    print('PASS malformed entries are refreshed',flush=True)

    changed_target=project('different-target',libs)
    output,calls=run(changed_target,target='1.25.4')
    assert len(version_queries(calls))==4 and len(metadata_queries(calls))==4,calls
    changed_resolver=project('different-resolver',libs)
    output,calls=run(changed_resolver,extra_env={'GONOSUMDB':'example.com/*'})
    assert len(version_queries(calls))==4 and len(metadata_queries(calls))==4,calls
    print('PASS target/toolchain resolver scope prevents incompatible cache reuse',flush=True)

    fault_cache=str(root/'fault-cache')
    fault=project('metadata-network-error',libs)
    output,calls=run(fault,extra_env={'UPGRADE_GO_CACHE_DIR':fault_cache,'FAIL_METADATA':'true'})
    assert not list(pathlib.Path(fault_cache).glob('**/metadata/*'))
    repaired=project('metadata-network-recovered',libs)
    output,calls=run(repaired,extra_env={'UPGRADE_GO_CACHE_DIR':fault_cache})
    assert not version_queries(calls) and len(metadata_queries(calls))==4,calls
    print('PASS transient metadata failures are not shared as incompatibilities',flush=True)

    version_fault_cache=str(root/'version-fault-cache')
    fault=project('version-network-error',libs)
    output,calls=run(fault,extra_env={'UPGRADE_GO_CACHE_DIR':version_fault_cache,'FAIL_VERSIONS':'true'})
    assert not list(pathlib.Path(version_fault_cache).glob('**/versions/*'))
    repaired=project('version-network-recovered',libs)
    output,calls=run(repaired,extra_env={'UPGRADE_GO_CACHE_DIR':version_fault_cache})
    assert len(version_queries(calls))==4
    print('PASS transient version lookup failures are retried by the next project',flush=True)

    # A cached successful dependency version cannot prove another project's API compatibility.
    api='example.com/shared/api'
    publish(api,'v1.0.0','package lib\nfunc Value() int {return 1}\nfunc Old() int {return 1}\n')
    publish(api,'v1.1.0','package lib\nfunc Value() int {return 2}\n')
    api_a=project('api-a',[api]); output,calls=run(api_a)
    api_b=project('api-b',[api])
    (api_b/'main.go').write_text(f'package main\nimport lib "{api}"\nfunc main() {{println(lib.Old())}}\n')
    output,calls=run(api_b)
    assert not version_queries(calls) and not metadata_queries(calls)
    assert api+' v1.0.0' in (api_b/'go.mod').read_text()
    assert 'Retained' in output
    print('PASS warm cache still catches project B API incompatibilities',flush=True)

    # Independent projects may publish the same cache entries concurrently.
    parallel_cache=str(root/'parallel-cache')
    processes=[]
    for index in range(2):
        p=project(f'parallel-{index}',libs)
        trace=p/'parallel-trace'; trace.write_text('')
        env=dict(base_env,GO_TRACE=str(trace),PATH=str(wrapper_dir)+':'+os.environ['PATH'],UPGRADE_GO_CACHE_DIR=parallel_cache)
        processes.append((p,subprocess.Popen([SCRIPT,'1.25.3','--skip-tests','--skip-vet','--upgrade-deps'],cwd=p,env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)))
    for p,proc in processes:
        out,err=proc.communicate()
        assert proc.returncode==0,(out,err)
        assert libs[0]+' v1.2.0' in (p/'go.mod').read_text()
    assert not list(pathlib.Path(parallel_cache).rglob('*.tmp.*'))
    assert all(p.read_text().splitlines()[0] in ('0','1') for p in pathlib.Path(parallel_cache).glob('**/metadata/*'))
    print('PASS concurrent cache publication stays complete and consistent',flush=True)
