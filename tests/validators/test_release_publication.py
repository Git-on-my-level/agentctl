#!/usr/bin/env python3
"""Exercise publication with a fake gh; no network, token, or real release."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
FAKE_GH = r'''#!/usr/bin/env python3
import json,os,shutil,sys
from pathlib import Path
args=sys.argv[1:]
with open(os.environ["GH_FIXTURE_LOG"],"a") as log: log.write(json.dumps(args)+"\n")
remote=Path(os.environ["GH_FIXTURE_REMOTE"])
mode=os.environ["GH_FIXTURE_MODE"]
if args[:2]==["release","view"]:
 if mode=="absent": sys.exit(1)
 if "--json" in args:
  if mode=="listing-error": sys.exit(2)
  print("\n".join(sorted(p.name for p in remote.iterdir())))
elif args[:2]==["release","download"]:
 name=args[args.index("--pattern")+1]
 shutil.copyfile(remote/name,Path(args[args.index("--dir")+1])/name)
elif args[:2] not in (["release","create"],["release","upload"]):
 sys.exit(3)
'''

def run_case(mode, existing=(), mismatch=False, corrupt=False, tag="v0.12.0"):
    with tempfile.TemporaryDirectory() as temp:
        root = Path(temp)
        dist, remote, binary = root / "dist", root / "remote", root / "bin"
        for folder in (dist, remote, binary):
            folder.mkdir()
        archive = f"agentctl_{tag}_linux_amd64.tar.gz"
        data = b"fixture archive bytes"
        (dist / archive).write_bytes(data)
        (dist / "SHA256SUMS").write_text(hashlib.sha256(data).hexdigest() + "  " + archive + "\n")
        for name in existing:
            name = archive if name == "archive" else name
            (remote / name).write_bytes((dist / name).read_bytes())
        if mismatch:
            (remote / archive).write_bytes(b"different existing bytes")
        if corrupt:
            (dist / archive).write_bytes(b"corrupted local archive")
        gh = binary / "gh"
        gh.write_text(FAKE_GH)
        gh.chmod(0o700)
        log = root / "calls.jsonl"
        env = dict(os.environ, PATH=str(binary) + os.pathsep + os.environ["PATH"], GH_FIXTURE_MODE=mode,
                   GH_FIXTURE_REMOTE=str(remote), GH_FIXTURE_LOG=str(log))
        result = subprocess.run([str(ROOT / "scripts/publish-release.sh"), tag, str(dist)], env=env,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
        mutations = [call for call in calls if call[:2] in (["release", "create"], ["release", "upload"])]
        assert all("--clobber" not in call for call in calls), calls
        return result.returncode, calls, mutations

code, calls, writes = run_case("absent")
assert code == 0 and len(writes) == 1 and writes[0][:2] == ["release", "create"], (code, calls)
assert "--verify-tag" in writes[0] and "--generate-notes" in writes[0] and "--notes-file" in writes[0]
code, calls, writes = run_case("absent", tag="v9.9.9")
assert code == 0 and "--notes-file" not in writes[0]
code, calls, writes = run_case("existing")
assert code == 0 and len(writes) == 1 and writes[0][:2] == ["release", "upload"] and len(writes[0]) == 5
code, calls, writes = run_case("existing", existing=("archive",))
assert code == 0 and len(writes) == 1 and len(writes[0]) == 4 and writes[0][-1].endswith("SHA256SUMS")
assert any(call[:2] == ["release", "download"] for call in calls)
code, calls, writes = run_case("existing", existing=("archive", "SHA256SUMS"))
assert code == 0 and not writes, (code, calls)
for kwargs in ({"existing": ("archive",), "mismatch": True}, {"corrupt": True}):
    code, calls, writes = run_case("existing", **kwargs)
    assert code != 0 and not writes, (code, calls)
code, calls, writes = run_case("listing-error")
assert code != 0 and not writes, (code, calls)
print("ok: release publication verifies bytes, preserves existing state, and uploads only missing assets")
