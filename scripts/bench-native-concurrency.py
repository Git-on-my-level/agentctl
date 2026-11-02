"""Opt-in Unix load test using fake native processes and a fresh private journal.

Usage: python3 -I scripts/bench-native-concurrency.py BINARY NEW_DIRECTORY --agents 30
No model/provider requests are made. Run under the host's build/test concurrency
gate. Repeated metadata models quiet agents; --alternating-events adds a distinct
stream burst. Metrics separate initial admission, replay, status and recovery.
"""

import argparse
import fcntl
import hashlib
from concurrent.futures import ThreadPoolExecutor
import json
import math
import os
from pathlib import Path
import subprocess
import shutil
import threading
import time

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('binary')
p.add_argument('root')
p.add_argument('--agents', type=int, default=30)
p.add_argument('--require-clean', action='store_true')
p.add_argument('--alternating-events', action='store_true')
a = p.parse_args()
if not 1<=a.agents<=50:
    p.error('--agents must be between 1 and 50')
root = Path(a.root).resolve()
root.mkdir(mode=0o700, parents=True, exist_ok=False)
binary_snapshot=root/'agentctl-under-test'
shutil.copy2(a.binary,binary_snapshot)
binary_sha256=hashlib.sha256(binary_snapshot.read_bytes()).hexdigest()
state = root / 'state'
state.mkdir(mode=0o700)
(root/'ready').mkdir(mode=0o700)
if a.alternating_events:
    (root/'alternating').touch()
native = root / 'fake-codex'
native.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys, time
if '--version' in sys.argv:
    print('codex-cli 0.55.0'); sys.exit(0)
root = pathlib.Path(__file__).parent
with (root / 'owners').open('a') as f:
    f.write(str(os.getpid())+' '+str(os.getppid())+'\\n')
(root / 'ready' / str(os.getpid())).touch()
print(json.dumps({'type':'thread.started','thread_id':'fixture-thread'}), flush=True)
while not (root / 'burst').exists() and not (root / 'release').exists():
    time.sleep(.1)
for i in range(120):
    if (root / 'release').exists(): break
    kind='item.started' if (root/'alternating').exists() and i%2==0 else 'item.completed'
    print(json.dumps({'type':kind,'item':{'id':str(i),'type':'command_execution','status':'completed','command':'true','exit_code':0}}), flush=True)
    time.sleep(.05)
while not (root / 'release').exists(): time.sleep(.1)
print(json.dumps({'type':'item.completed','item':{'id':'answer','type':'agent_message','text':'fixture answer'}}), flush=True)
print(json.dumps({'type':'turn.completed'}), flush=True)
''')
native.chmod(0o700)
config = root / 'config.json'
config.write_text(json.dumps({'schema_version':1,'default_profile':'test','profiles':{'test':{'adapters':{'codex':{'executable':str(native)}},'agent_preferences':{'mode':'advisory','preferred':[{'agent':'codex','family':'test','model':'fixture-model'}]}}}}))
config.chmod(0o600)
env = dict(os.environ, AGENTCTL_UPDATE_MODE='off', AGENTCTL_STATE_HOME=str(state))
env.pop('AGENTCTL_CONFIG_BUNDLE', None)
env.pop('AGENTCTL_CONFIG', None)
env.pop('AGENTCTL_PROFILE', None)
# Build/test Go worker caps must not change the benchmarked CLI runtime
# between the original baseline and candidate workload.
env.pop('GOMAXPROCS', None)
base = [str(binary_snapshot),'--output','json','--config',str(config),'--journal',str(state/'journal.db')]
def invoke(args, prompt=None, timeout=150):
    started=time.monotonic()
    try:
        p=subprocess.run(base+args, input=prompt, text=True, capture_output=True, env=env, timeout=timeout)
    except subprocess.TimeoutExpired:
        return {'rc':124,'seconds':time.monotonic()-started,'data':{}}
    try: data=json.loads(p.stdout)
    except ValueError: data={}
    rc=p.returncode
    if args[0]!='await' and rc==0 and not data.get('ok'):
        rc=125
    return {'rc':rc,'seconds':time.monotonic()-started,'data':data}
def launch(i):
    request=root / ('request-%d.json'%i)
    request.write_text(json.dumps({'schema_version':1,'request_key':'load-%d'%i,'selector':{'family':'test'}}))
    return invoke(['delegate','--cwd',str(root),'--request-file',str(request),'--prompt-stdin','--detach','--timeout','120s'], 'synthetic fixture only')
polls=[]
cycles=[]
stop=threading.Event()
lock_windows=[]
lock_samples=0
def sample_lock():
    global lock_samples
    fd=os.open(state/'journal.db',os.O_RDONLY)
    blocked_at=None
    try:
        while not stop.is_set():
            now=time.monotonic()
            lock_samples+=1
            try:
                fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB)
                fcntl.flock(fd,fcntl.LOCK_UN)
                if blocked_at is not None:
                    lock_windows.append(now-blocked_at)
                    blocked_at=None
            except BlockingIOError:
                if blocked_at is None: blocked_at=now
            stop.wait(.001)
        if blocked_at is not None: lock_windows.append(time.monotonic()-blocked_at)
    finally:
        os.close(fd)
def poll():
    while not stop.is_set():
        polls.append(invoke(['recent','--summary'], timeout=45))
        stop.wait(.1)
def recover():
    while not stop.is_set():
        cycles.append(invoke(['supervisor','run','--once','--state-dir',str(state)], timeout=60))
        stop.wait(.5)
def summary(rows):
    lat=sorted(r['seconds'] for r in rows)
    errors=sum(r['rc']!=0 for r in rows)
    busy=sum(r['data'].get('error',{}).get('details',{}).get('diagnostic_code')=='journal_busy' for r in rows)
    return {'n':len(rows),'errors':errors,'journal_busy':busy,'p50_s':round(lat[len(lat)//2],4) if lat else 0,'p95_s':round(lat[min(len(lat)-1,math.ceil(.95*len(lat))-1)],4) if lat else 0}
threads=[]
launches=[]
ready_workers=0
ready_workers_at_receipts=0
readiness_seconds=0
try:
    # Initialize before concurrent cold opens, just as an existing journal.
    invoke(['supervisor','run','--once','--state-dir',str(state)])
    threads=[threading.Thread(target=poll),threading.Thread(target=recover),threading.Thread(target=sample_lock)]
    for thread in threads: thread.start()
    with ThreadPoolExecutor(max_workers=a.agents) as pool:
        launches=list(pool.map(launch,range(a.agents)))
    # A launch receipt proves process creation, not application readiness.
    # Hold the burst until every synthetic child reaches its startup marker.
    ready_workers_at_receipts=len(list((root/'ready').iterdir()))
    readiness_started=time.monotonic()
    readiness_deadline=readiness_started+30
    while True:
        ready_workers=len(list((root/'ready').iterdir()))
        if ready_workers==a.agents or time.monotonic()>=readiness_deadline:
            break
        stop.wait(.01)
    readiness_seconds=time.monotonic()-readiness_started
    (root/'burst').touch()
    stop.wait(20)
finally:
    (root/'release').touch()
    stop.set()
    for thread in threads: thread.join()
    # Replays recover every keyed row, including an absent/lost initial receipt.
    # Await each admitted execution so detached fixture owners are collected.
    replays=[]
    with ThreadPoolExecutor(max_workers=4) as pool:
        replays=list(pool.map(launch,range(a.agents)))
    ids=[r['data'].get('result',{}).get('id') or r['data'].get('error',{}).get('details',{}).get('execution_id') for r in launches+replays]
    await_rows=[]
    for eid in set(ids)-{None}:
        await_rows.append(invoke(['await',eid,'--timeout','130s','--content'],timeout=145))
    terminal=invoke(['recent','--summary'])
    result={'binary':a.binary,'agents':a.agents,'alternating_events':a.alternating_events,'launch':summary(launches),'status':summary(polls),'reprobe_cycle':summary(cycles),'recovered_ids':len(set(ids)-{None}),'await_errors':sum(r['rc']!=0 for r in await_rows),'terminal_summary':terminal['data'].get('result',{}).get('summary',{})}
    result['replay']=summary(replays)
    result['binary_sha256']=binary_sha256
    result['ready_workers_at_burst']=ready_workers
    result['ready_workers_at_receipts']=ready_workers_at_receipts
    result['readiness_wait_s']=round(readiness_seconds,4)
    failures=[f for cycle in cycles for f in cycle['data'].get('result',{}).get('recovery',{}).get('failures',[])]
    result['reprobe_cycle']['probe_failures']=len(failures)
    result['reprobe_cycle']['journal_busy_failures']=sum('journal busy' in f.get('error','') for f in failures)
    result['reprobe_cycle']['pending_binding_failures']=sum('no native source binding' in f.get('error','') for f in failures)
    result['lock_unavailable_1ms_sample']={'samples':lock_samples,'windows':summary([{'rc':0,'seconds':w,'data':{}} for w in lock_windows]),'max_s':round(max(lock_windows,default=0),4)}
    (root/'metrics.json').write_text(json.dumps(result,indent=2)+'\n')
    print(json.dumps(result,indent=2))
    if a.require_clean and (result['launch']['errors'] or result['replay']['errors'] or result['status']['errors'] or result['reprobe_cycle']['errors'] or result['reprobe_cycle']['probe_failures'] or result['await_errors'] or result['recovered_ids']!=a.agents or ready_workers!=a.agents):
        raise SystemExit(1)
