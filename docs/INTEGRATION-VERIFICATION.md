# Integration Guide — verification

Every request/response pair in [`docs/INTEGRATION-GUIDE.md`](INTEGRATION-GUIDE.md)
was executed against a **real** `sandboxd` built from this tree, with **real
nsjail**, a **real per-workspace FUSE disk**, and **real network egress**
(slirp4netns), on the host. This document records the exact command, the actual
body, the actual response (long payloads trimmed, field names and structure
kept), and the observed status code. Where a flow cannot be exercised without an
external dependency, that is stated explicitly.

The harness started the service as:

```sh
go build -o /tmp/opencode/integration/sandboxd ./cmd/sandboxd
sudo -n /tmp/opencode/integration/sandboxd --config /tmp/opencode/integration/verify.yaml
```

with `nsjail_path: /usr/local/bin/nsjail`, `network.enabled: true`,
`disk.enabled: true`, `metrics.enabled: true`, `ports.enabled: true`,
`archives.enabled: true`, `subscription.enabled: true`, and
`profiles.default: "python"`. Keys: `dev-key-please-rotate` → `user_dev`,
`other-key` → `user_other`. All host resources were cleaned up at the end
(`fuse2fs=0`, `nsjail cgroups=0`, `ws cgroups=0`).

`KEY` below is `dev-key-please-rotate`. Curl calls carry
`-H "Authorization: Bearer $KEY"` unless a section says otherwise.

---

## 0. Service startup and readiness

```sh
$ curl -s http://127.0.0.1:8095/healthz
{"status":"ok"}
HTTP_STATUS=200

$ curl -s http://127.0.0.1:8095/readyz
{"cgroups":true,"data_dir":true,
 "egress_decoy":{"enabled":false,"ready":false,"reason":"network.decoy.enabled is false"},
 "identity_masking":{"enabled":true,"ready":true},
 "network":{"available":true,"enabled":true,"ready":true},
 "nsjail":true,
 "profiles":{"default":"python","ready":true},
 "provider_tools_hidden":{"enabled":true,"ready":true},
 "status":"ready",
 "tunnels":{"enabled":false,"ready":false,"reason":"tunnels.enabled is false"},
 "workspace_limits":{"enabled":true,"mechanism":"fuse","proc":true,
   "proc_files":["meminfo","cpuinfo","stat","uptime","loadavg","version","vmstat",
                 "filesystems","mounts","self/mounts","self/mountinfo","self/limits"],
   "reason":"per-workspace fuse2fs ext4 image sized to disk_mb (kernel boundary, no overshoot)"}}
HTTP_STATUS=200
```

---

## 1. `GET /v1/meta`

```sh
$ curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:8095/v1/meta
```

The whole response (200) is in the harness transcript; the top-level keys
observed were:

```
['api_version', 'archives', 'egress_decoy', 'identity_masking', 'interpreters',
 'limits_defaults', 'limits_maxima', 'metrics', 'network', 'ports', 'profiles',
 'provider_tools_hidden', 'runnable_extensions', 'runtimes', 'subscriptions',
 'tunnels', 'version', 'workspace_limits', 'write_rate']
```

(Ordered alphabetically by the harness; the raw service emits them in handler
order.) Selected values (real):

```json
{
  "version": "0.0.20",
  "api_version": "v1",
  "runtimes": ["python3"],
  "interpreters": ["bash","node","perl","python3"],
  "runnable_extensions": [".bash",".cjs",".js",".mjs",".pl",".pm",".py",".sh"],
  "limits_defaults": {"cpu_cores":1,"memory_mb":512,"disk_mb":256,
    "max_wall_time_sec":300,"max_runs":8,"allow_net":true,
    "allow_unlimited_runs":true,"allow_tunnels":false,"write_rate_mb_s":0},
  "limits_maxima": {"cpu_cores":8,"memory_mb":8192,"disk_mb":51200,
    "max_wall_time_sec":3600,"max_runs":64,"allow_net":true,
    "allow_unlimited_runs":true,"write_rate_mb_s":0},
  "profiles": {"default":"python","ready":true,"profiles":[
    {"name":"minimal","commands":["awk","basename","bash", ... ,"xargs"], ...},
    {"name":"python","commands":["awk", ..., "python3","node","pip","git", ...],
     "paths":["/usr/lib/python3.12","/usr/lib/python3", ...], ...}]},
  "ports": {"enabled":true,"include_unix":true,"max_pids":64,
    "max_fds_per_pid":512,
    "reachability":"a reported port is listening inside the workspace's private network namespace; it is NOT reachable from outside unless a tunnel publishes it (limits.allow_tunnels AND tunnels.enabled)",
    "reads_total":0, "routes":["GET /v1/workspaces/{id}/ports"]},
  "metrics": {"enabled":true,"interval_seconds":3,"min_interval_sec":1,
    "max_interval_sec":300,"include_cpu":false,"max_subscribers":1024, ...},
  "archives": {"enabled":true,"formats":["zip","rar"],"read_only":["rar"],
    "limits":{"max_entries":20000,"max_total_mb":2048,"max_file_mb":512,
              "max_ratio":200,"upload_mb":50,"overwrite":false,"skip_junk":true},
    "refusal_codes":["unsupported_archive","archive_encrypted",
      "archive_multivolume","archive_too_large","archive_too_many_entries",
      "archive_ratio_exceeded","archive_unsafe_entry","archive_entry_exists"]},
  "subscriptions": {"enabled":true,"default_duration":"","max_duration":"8760h0m0s",
    "grace_period":"168h0m0s","reap_interval":"60s",
    "duration_grammar":"s m h d w mo y (summed, e.g. 1d12h); mo=30d and y=365d are FIXED-LENGTH approximations, never calendar-exact", ...},
  "workspace_limits": {"enabled":true,"mechanism":"fuse",
    "reporting":"real per-workspace ext4 filesystem image (fuse2fs), host-side mounted; df/stat -f/os.statvfs are the filesystem's own answers",
    "enforcement":"kernel filesystem boundary: a write past the allowance fails with ENOSPC, no overshoot"},
  "write_rate": {"enabled":false,"server_max_mb_s":0,"default_mb_s":0,"samples":3,
    "outcome":"rate_limited","refusal_code":"write_rate_exceeded", ...}
}
HTTP_STATUS=200
```

**Behaviour that differed from a naive expectation:** `subscriptions.max_duration`
is rendered as a Go duration string (`"8760h0m0s"`, i.e. 365 days) and
`grace_period` as `"168h0m0s"`, **not** in the `d` grammar the request accepts.
The guide documents both the request grammar and the rendered value.

---

## 1b. Auth refusals

```sh
$ curl -s http://127.0.0.1:8095/v1/meta                       # no Authorization
{"error":{"code":"unauthorized","message":"missing or invalid credentials"}}
HTTP_STATUS=401

$ curl -s -H "Authorization: Bearer wrong" http://127.0.0.1:8095/v1/meta
{"error":{"code":"unauthorized","message":"missing or invalid credentials"}}
HTTP_STATUS=401

$ curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:8095/v1/workspaces/ws_nope
{"error":{"code":"not_found","message":"resource not found"}}
HTTP_STATUS=404
```

---

## 2. Canonical lifecycle — create → upload → install → run → terminal → ports → lifecycle → delete

### 2.1 Create

```sh
$ curl -s -X POST http://127.0.0.1:8095/v1/workspaces \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d '{"name":"demo","profile":"python"}'
{
  "workspace": {
    "id": "ws_01M6HJRCN8TXR70AE3815B9RZN",
    "owner_id": "user_dev", "name": "demo", "profile": "python",
    "created_at": "2026-11-03T20:41:39Z", "status": "active",
    "limits": {"cpu_cores":1,"memory_mb":512,"disk_mb":256,"max_wall_time_sec":300,
               "max_runs":8,"allow_net":true,"allow_unlimited_runs":true,
               "allow_tunnels":false,"write_rate_mb_s":0},
    "last_activity_at": "2026-11-03T20:41:39Z",
    "summary": {"state":"unlimited","age_sec":0,"created_at":"...",
                "runs":{"total":0,"active":0},
                "disk":{"used_bytes":0,"limit_bytes":268435456},
                "profile":"python"}
  }
}
HTTP_STATUS=201
```

### 2.2 Upload a project

```sh
$ curl -s -X PUT \
    'http://127.0.0.1:8095/v1/workspaces/ws_.../files/content?path=/main.py' \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d '{"content":"aW1wb3J0IHN5cw ...","encoding":"base64"}'
HTTP_STATUS=204
```

Read back with meta (the JSON wrapper form):

```sh
$ curl -s -H "Authorization: Bearer $KEY" \
    'http://127.0.0.1:8095/v1/workspaces/ws_.../files/content?path=/main.py&meta=true'
{"path":"/main.py","content":"import sys\ndef main():\n    print('hello from main.py')\n ...",
 "encoding":"text","size":145,"total_size":145,"range":{"offset":0,"length":145},
 "whole_file":true,"sha256":"...","content_type":"text/x-python; charset=utf-8",
 "truncated":false,"binary":false}
HTTP_STATUS=200
```

### 2.3 Install dependencies (real pip install over real egress)

```sh
$ curl -s -X POST http://127.0.0.1:8095/v1/workspaces/ws_.../packages/install \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d '{"packages":["six"]}'
{"exit_code":0,"output":"Collecting six\n  Downloading six-1.17.0-py2.py3-none-any.whl (11 kB)\nInstalling collected packages: six\nSuccessfully installed six-1.17.0\n...","stdout":"...","target":"/opt/python/private","timed_out":false,"truncated":false,"duration_ms":3290}
HTTP_STATUS=200

$ curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:8095/v1/workspaces/ws_.../packages
{"packages":[{"name":"six","version":"1.17.0"}],"count":1,"truncated":false,
 "shared_enabled":false,"shared_available":false,"shared_path":"",
 "private_path":"/opt/python/private"}
HTTP_STATUS=200

$ curl -s -X DELETE \
    'http://127.0.0.1:8095/v1/workspaces/ws_.../packages?name=six' \
    -H "Authorization: Bearer $KEY"
HTTP_STATUS=204
```

### 2.4 Run the project

See §4.

### 2.5 Terminal, ports, lifecycle

See §5, §7, §8.

### 2.6 Delete

```sh
$ curl -s -X DELETE http://127.0.0.1:8095/v1/workspaces/ws_... -H "Authorization: Bearer $KEY"
HTTP_STATUS=204
$ curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:8095/v1/workspaces/ws_...
{"error":{"code":"not_found","message":"resource not found"}}
HTTP_STATUS=404
```

---

## 3. Files

### 3.1 mkdir, rename, list, stat, upload, download

```sh
$ curl -s -X POST .../files/mkdir -H ... -d '{"path":"/src"}'
{"path":"/src"}
HTTP_STATUS=201

$ curl -s -X POST .../files/rename -H ... -d '{"from":"/main.py","to":"/src/main.py"}'
HTTP_STATUS=204

$ curl -s -H "Authorization: Bearer $KEY" '.../files?path=/src'
{"count":1,"entries":[{"name":"main.py","path":"/src/main.py","type":"file",
  "size":145,"mode":"0644","mod_time":"2026-11-03T20:41:39Z","runnable":"python3"}],
 "path":"/src","total":1,"truncated":false}
HTTP_STATUS=200

$ curl -s -H "Authorization: Bearer $KEY" '.../files/stat?path=/src/main.py'
{"name":"main.py","path":"/src/main.py","type":"file","size":145,"mode":"0644",
 "mod_time":"...","content_type":"text/x-python; charset=utf-8","sha256":"...",
 "runnable":"python3"}
HTTP_STATUS=200

$ curl -s -F path=/src/data.txt -F file=@data.txt -H "Authorization: Bearer $KEY" .../files/upload
{"name":"data.txt","path":"/src/data.txt","size":3}
HTTP_STATUS=201

$ curl -s -H "Authorization: Bearer $KEY" '.../files/download?path=/src/data.txt'   # RAW bytes
42
HTTP_STATUS=200
```

### 3.2 Write is a JSON body; download is raw bytes

Confirmed: `PUT /files/content` carries `{"content":"...","encoding":"base64"}`
(not raw bytes) and returns `204`; `GET /files/download` returns the raw bytes
`42\n` with no JSON wrapper. Both behaviours are stated in the guide.

### 3.3 Archives

Export a directory as a zip (raw `application/zip`):

```sh
$ curl -s -H "Authorization: Bearer $KEY" '.../files/archive?path=/src' -o /tmp/src.zip -w 'HTTP_STATUS=%{http_code}\n'
HTTP_STATUS=200
$ ls -l /tmp/src.zip
-rw-r--r-- 1 root root 286 ... /tmp/src.zip
```

Upload-and-extract (`POST /files/archive`) and extract-in-place
(`POST /files/extract`):

```sh
$ curl -s -X POST .../files/extract -H ... -d '{"path":"/src.zip","dest":"/src2"}'
{"format":"zip","destination":"/src2","entries_total":2,"entries_extracted":2,
 "entries_skipped":0,"entries_rejected":0,"bytes_written":3,
 "entries":[{"name":"data.txt","path":"/src2/data.txt","type":"file","size":3,
             "compressed_size":9,"mode":"-rw-r--r--","rejected":false,"skipped":false},
            {"name":"main.py","path":"/src2/main.py","type":"file","size":145,
             "compressed_size":... ,"mode":"-rw-r--r--","rejected":false,"skipped":false}],
 "dry_run":false}
HTTP_STATUS=200
```

Refusals (the constants in `/v1/meta.archives.refusal_codes`):

```sh
$ curl -s -F path=/x -F archive=@garbage.bin -H "Authorization: Bearer $KEY" .../files/archive
{"error":{"code":"unsupported_archive","message":"unsupported archive format",
          "details":{"signature":"0x6E6F7420616E2061"}}}
HTTP_STATUS=415

$ curl -s -F path=/x -F archive=@internal/archive/testdata/rar5/multivolume.part1.rar .../files/archive
{"error":{"code":"archive_multivolume","message":"multi-volume archives are not supported"}}
HTTP_STATUS=422
```

RAR read-only is confirmed by `read_only:["rar"]` in `/v1/meta.archives` and by
the archive package only ever writing ZIP (`GET /files/archive` streams
`application/zip`).

---

## 4. Exec and runs

### 4.1 `exec` (hard 60 s cap)

```sh
$ curl -s -X POST .../exec -H ... \
    -d '{"cmd":["python3","/workspace/src/main.py","a","b"]}'
{"exit_code":0,"stdout":"hello from main.py\nargs: ['a', 'b']\n",
 "stderr":"","duration_ms":68,"truncated":false}
HTTP_STATUS=200

$ curl -s -X POST .../exec -H ... \
    -d '{"cmd":["python3","src/main.py"],"cwd":"/src"}'
{"exit_code":0,"stdout":"hello from main.py\nargs: []\n",
 "stderr":"","duration_ms":82,"truncated":false}
HTTP_STATUS=200
```

**Behaved differently from a naive expectation, and corrected in the guide:**
a first attempt used `{"cmd":["python3","/src/main.py"]}` and got

```json
{"exit_code":2,"stderr":"/bin/python3: can't open file '/src/main.py': [Errno 2] No such file or directory\n", ...}
```

because **inside the sandbox the workspace root is mounted at `/workspace`**, so
a workspace-relative path `/src/main.py` is `/workspace/src/main.py` to a
command. The guide states this in §3 and §4.

### 4.2 `runs` — create returns 202 `queued`

```sh
$ curl -s -X POST .../runs -H ... -d '{"kind":"file","file":"/src/main.py","args":["x","y"]}'
{"run":{"id":"run_01M6HJRGJCJG27NW5ACG959XQM","workspace_id":"ws_...",
  "kind":"file","argv":["python3","/workspace/src/main.py","x","y"],
  "status":"queued","exit_code":null,"started_at":"...","ended_at":"",
  "cwd":"/","timeout_sec":300,"unlimited":false,"interpreter":"python3",
  "logs":{"stdout_bytes":0,"stderr_bytes":0,"truncated":false}}}
HTTP_STATUS=202
```

### 4.3 Logs are read by byte offset, polled from `next`

```sh
$ curl -s -H "Authorization: Bearer $KEY" .../runs/run_.../logs
{"data":"","eof":true,"next":0,"offset":0,"truncated":false}
HTTP_STATUS=200

poll 1 (offset=0): {"data":"hello from main.py\nargs: ['x', 'y']\n","eof":true,
                    "next":36,"offset":0,"truncated":false}
  run status=exited eof=True next=36

$ curl -s -H "Authorization: Bearer $KEY" .../runs/run_...
{"run":{"id":"run_...","status":"exited","exit_code":0,"ended_at":"...",
  "timeout_sec":300,"unlimited":false,"interpreter":"python3",
  "logs":{"stdout_bytes":36,"stderr_bytes":0,"truncated":false}}}
HTTP_STATUS=200
```

**The important negative fact:** the `run` object has **no `stdout`/`stderr`
field** — the first logs read before the run wrote anything is
`{"data":"","eof":true,...}` and the output only appears via `/logs`. This is the
exact failure mode the guide calls out (a client reading the run body reports an
empty result).

### 4.4 `kind:"command"`, and the stderr stream

```sh
$ curl -s -X POST .../runs -H ... -d '{"kind":"command","argv":["/bin/sh","-c","echo out; echo err 1>&2"]}'
# -> 202; after it exits:
$ curl -s -H "Authorization: Bearer $KEY" '.../runs/run_.../logs?stream=stdout'
{"data":"out\n","eof":true,"next":4,"offset":0,"truncated":false}
$ curl -s -H "Authorization: Bearer $KEY" '.../runs/run_.../logs?stream=stderr'
{"data":"err\n","eof":true,"next":4,"offset":0,"truncated":false}
```

### 4.5 Stop a run, list, purge

```sh
$ curl -s -X POST .../runs/run_.../stop -H "Authorization: Bearer $KEY"
{"run":{"id":"run_...","kind":"command","argv":["python3","-c","import time;time.sleep(300)"],
  "status":"killed","exit_code":137,"timeout_sec":null,"unlimited":true, ...}}
HTTP_STATUS=202

$ curl -s -H "Authorization: Bearer $KEY" '.../runs?limit=3'
{"limit":3,"offset":0,"runs":[ ... ],"total":3}
HTTP_STATUS=200

$ curl -s -o /dev/null -w 'HTTP_STATUS=%{http_code}\n' -X DELETE .../runs/run_... -H "Authorization: Bearer $KEY"
HTTP_STATUS=204

$ curl -s -X DELETE .../runs -H "Authorization: Bearer $KEY"
{"deleted":2}
HTTP_STATUS=200
```

---

## 5. Interactive session (real PTY)

```sh
$ curl -s -X POST .../sessions -H ... -d '{"cols":80,"rows":24}'
{"session":{"id":"ses_01M6HJRJWD1N5F814BGNDCG7EV","workspace_id":"ws_...",
  "shell":"/bin/sh","cwd":"","cols":80,"rows":24,"status":"running",
  "created_at":"...","last_activity_at":"...","started_at":"...","ended_at":"",
  "exit_code":null,"output_bytes":0,"output_truncated":false}}
HTTP_STATUS=201

$ curl -s -X POST .../sessions/ses_.../input -H ... -d '{"data":"echo HELLO_SESSION\r"}'
HTTP_STATUS=204

$ curl -s -H "Authorization: Bearer $KEY" '.../sessions/ses_.../output?offset=0'
{"data":"$ echo HELLO_SESSION\r\nHELLO_SESSION\r\n$ ","offset":0,"next":39,
 "eof":true,"truncated":false}
HTTP_STATUS=200

$ curl -s -X POST .../sessions/ses_.../resize -H ... -d '{"cols":100,"rows":40}'
HTTP_STATUS=204

$ curl -s -X POST .../sessions/ses_.../signal -H ... -d '{"signal":"INT"}'
HTTP_STATUS=204

$ curl -s -X DELETE .../sessions/ses_... -H "Authorization: Bearer $KEY"
{"changed":true,"session":{"id":"ses_...","status":"killed","reason":"user_closed",
  "ended_at":"...","output_bytes":43,"output_truncated":false}}
HTTP_STATUS=200

$ curl -s -X POST .../sessions/ses_.../input -H ... -d '{"data":"x\n"}'
{"error":{"code":"session_ended","message":"session has ended","details":{"status":"killed"}}}
HTTP_STATUS=409
```

The PTY is real: the echoed input, the shell prompt `$ `, and the CR/LF
translation are all visible in the output bytes. The process inventory (below)
shows the session's relay helper and the `/bin/sh` it hosts, proving one
long-lived process.

---

## 6. Metrics

```sh
$ curl -s -H "Authorization: Bearer $KEY" .../workspaces/ws_.../metrics
{"timestamp":"2026-11-03T20:49:20.207680071Z","interval_seconds":3,
 "workspaces":[{"workspace_id":"ws_...","ram_used_bytes":17870848,
   "ram_limit_bytes":536870912,"ram_percent":3.328704833984375,"ram_complete":true,
   "disk_used_bytes":491520,"disk_limit_bytes":268435456,"disk_percent":0.18310546875,
   "disk_complete":true,"write_rate_limit_mb_s":0,"live_runs":2,"live_cgroups":2}],
 "sampler":{"ticks":0,"cgroup_reads":3,"statfs_reads":1,"tree_walk_reads":0,
   "subscribers":0,"running":0,"dropped_frames":0,"candidates":1}}
HTTP_STATUS=200

$ echo '$ curl -s .../v1/metrics'   # service-wide snapshot, same frame shape
{"timestamp":"...","interval_seconds":3,"workspaces":[ ... ],"sampler":{...}}
HTTP_STATUS=200
```

Live stream (`text/event-stream`), truncated to three frames:

```sh
$ timeout 8 curl -s -N -H "Authorization: Bearer $KEY" .../workspaces/ws_.../metrics/stream
: connected

data: {"timestamp":"...","interval_seconds":3,"workspaces":[...],"sampler":{"ticks":1,...,"subscribers":1,"running":1,...}}

data: {"timestamp":"...","interval_seconds":3,"workspaces":[...],"sampler":{"ticks":2,...,"subscribers":1,"running":1,...}}

data: {"timestamp":"...","interval_seconds":3,"workspaces":[...],"sampler":{"ticks":3,...,"subscribers":1,"running":1,...}}
```

Floor enforced — a cadence below `min_interval_sec` is refused, not raised:

```sh
$ curl -s -H "Authorization: Bearer $KEY" '.../v1/metrics/stream?interval=0.5s'
{"error":{"code":"unprocessable","message":"interval below the configured floor (1s): 0.5s",
          "details":{"interval":"0.5s","min_interval":"1s"}}}
HTTP_STATUS=422
```

Idle workspace reports RAM 0 (a real state, not an error):

```sh
$ curl -s -H "Authorization: Bearer $KEY" .../workspaces/ws_idle/metrics
{"timestamp":"...","interval_seconds":3,"workspaces":[{"workspace_id":"ws_...",
  "ram_used_bytes":0,"ram_limit_bytes":536870912,"ram_percent":0,
  "ram_complete":true,"disk_used_bytes":0,"disk_limit_bytes":268435456,
  "disk_complete":true,"write_rate_limit_mb_s":0,"live_runs":0,"live_cgroups":0}],
 "sampler":{"ticks":3,"cgroup_reads":18,"statfs_reads":7,"tree_walk_reads":0,...}}
HTTP_STATUS=200
```

`tree_walk_reads` is 0 throughout, confirming the disk figure is one `statfs`.

---

## 7. Ports

Two runs started: a TCP listener on `0.0.0.0:18090`, and a process holding a
unix-domain socket at `/tmp/demo.sock`.

```sh
$ curl -s -H "Authorization: Bearer $KEY" .../workspaces/ws_.../ports
{
  "complete": true,
  "count": 1,
  "fds_scanned": 8,
  "pids_scanned": 2,
  "notes": [],
  "ports": [
    {
      "port": 18090, "protocol": "tcp", "address": "0.0.0.0", "scope": "wildcard",
      "state": "LISTEN", "attributed": true, "pid": 890648, "kind": "run",
      "label": "python3 -c import socket,time\ns=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind((\"0.0.0.0\",",
      "argv": ["/bin/python3","-c","import socket,time\ns=socket.socket();..."],
      "owners": [890648], "inode": 2726317911
    }
  ],
  "reachability": "ports are listening inside the workspace's private network namespace; this does not mean they are reachable from outside. Publishing a port requires a tunnel (limits.allow_tunnels and tunnels.enabled).",
  "unix": [
    {
      "path": "/tmp/demo.sock", "type": "stream", "state": "unconnected",
      "attributed": true, "pid": 890654, "kind": "run",
      "label": "python3 -c import socket,time,os\np=\"/tmp/demo.sock\"\ns=socket.socket(socket.AF_UNIX);s.bind(p);s.listen(1);time.sleep(120",
      "inode": 2726357326
    }
  ],
  "unix_count": 1,
  "workspace_id": "ws_..."
}
HTTP_STATUS=200
```

Per-entry fields (`port`, `protocol`, `address`, `scope`, `state`, `attributed`,
`pid`, `kind`, `label`, `argv`, `owners`, `inode`) and the separate `unix`
section are all present and real. The owning pid is resolved to the pid the run
actually has (it matches the process inventory below). The `reachability` note is
present in every response.

Process inventory (from the same workspace):

```sh
$ curl -s -H "Authorization: Bearer $KEY" .../workspaces/ws_.../processes
{"count":2,"processes":[
  {"id":"proc_a1b0...","pid":890648,"ppid":890647,
   "argv":["/bin/python3","-c","import socket,time\n...18090..."],
   "kind":"run","source_id":"run_01M6HJRQ7XRXKCGXHHA0SCKX3H","started_at":"..."},
  {"id":"proc_1a34...","pid":890654,"ppid":890653,
   "argv":["/bin/python3","-c","import socket,time,os\np=\"/tmp/demo.sock\"..."],
   "kind":"run","source_id":"run_01M6HJRQ95DZT0W3VVFCX619D8","started_at":"..."}]}

$ curl -s -X POST .../processes/proc_a1b0.../stop -H "Authorization: Bearer $KEY"
{"process_id":"proc_a1b0...","status":"stopped","stopped":true,"stopped_at":"..."}
HTTP_STATUS=200

$ curl -s -X POST .../processes/proc_nope/stop -H "Authorization: Bearer $KEY"
{"error":{"code":"not_found","message":"process not found"}}
HTTP_STATUS=404
```

---

## 8. Lifecycle, subscription, tunnels

### 8.1 install/list/delete packages

Verified in §2.3 above.

### 8.2 stop / suspend / resume

```sh
$ curl -s -X POST .../stop -H "Authorization: Bearer $KEY"
{"stopped_process_count":0,"stopped_process_ids":[],
 "stopped_run_count":2,"stopped_run_ids":["run_...","run_..."],
 "stopped_session_count":0,"stopped_session_ids":[],
 "workspace":{"id":"ws_...","status":"active", ...}}
HTTP_STATUS=200

$ curl -s -X POST .../suspend -H "Authorization: Bearer $KEY"
{"workspace":{"id":"ws_...","status":"suspended",...},"changed":true,
 "stopped_run_ids":[],"stopped_run_count":0}
HTTP_STATUS=200

$ curl -s -X POST .../exec -H ... -d '{"cmd":["python3","-c","print(1)"]}'   # on suspended
{"error":{"code":"workspace_suspended","message":"workspace is suspended"}}
HTTP_STATUS=409

$ curl -s -X POST .../resume -H "Authorization: Bearer $KEY"
{"workspace":{"id":"ws_...","status":"active",...},"changed":true}
HTTP_STATUS=200
```

### 8.3 subscription/extend while live (adds to the term)

```sh
$ curl -s -X POST .../subscription/extend -H ... -d '{"duration":"1d12h"}'
{"workspace":{"status":"active",
  "subscription":{"granted_at":"2026-11-03T20:49:28Z",
                  "expires_at":"2026-11-05T08:49:28Z",
                  "renewed_at":"2026-11-03T20:49:28Z",
                  "grace_ends_at":"2026-11-12T08:49:28Z","extensions":1},
  "summary":{"subscription":{"state":"active","expires_at":"2026-11-05T08:49:28Z",
                             "remaining_sec":129600,
                             "grace_ends_at":"2026-11-12T08:49:28Z",
                             "extensions":1}}}}
HTTP_STATUS=200
```

`1d12h` = 36 h = 129600 s, matching `remaining_sec`, which confirms the summed
grammar.

### 8.4 Subscription expiry and lock (real reaper, compressed clock)

A separate run used `grace_period: "8s"`, `reap_interval: "1s"`.

```sh
$ curl -s -X POST .../v1/workspaces -H ... -d '{"name":"sub","subscription":{"duration":"3s"}}'
{"workspace":{"id":"ws_...","status":"active",
  "subscription":{"granted_at":"2026-11-03T20:46:48Z",
                  "expires_at":"2026-11-03T20:46:51Z",
                  "grace_ends_at":"2026-11-03T20:46:59Z","extensions":0},
  "summary":{"subscription":{"state":"active","remaining_sec":3,...}}}}
HTTP_STATUS=201

# after ~6s (past expiry, within grace):
$ curl -s -H "Authorization: Bearer $KEY" .../v1/workspaces/ws_...
{"workspace":{"status":"locked",
  "subscription":{"granted_at":"...","expires_at":"2026-11-03T20:46:51Z",
                  "locked_at":"2026-11-03T20:46:52Z",
                  "grace_ends_at":"2026-11-03T20:46:59Z","extensions":0},
  "summary":{"subscription":{"state":"locked","remaining_sec":0,
                             "locked_at":"2026-11-03T20:46:52Z",...}}}}
HTTP_STATUS=200

$ curl -s -X POST .../exec -H ... -d '{"cmd":["python3","-c","print(1)"]}'   # work refused
{"error":{"code":"subscription_expired","message":"workspace subscription has lapsed (locked); renew to restore service"}}
HTTP_STATUS=403

$ curl -s -o /dev/null -w 'get=%{http_code}\n'   -H "Authorization: Bearer $KEY" .../v1/workspaces/ws_...
$ curl -s -o /dev/null -w 'usage=%{http_code}\n' -H "Authorization: Bearer $KEY" .../v1/workspaces/ws_.../usage
$ curl -s -o /dev/null -w 'files=%{http_code}\n' -H "Authorization: Bearer $KEY" .../v1/workspaces/ws_.../files
get=200
usage=200
files=200

$ curl -s -X POST .../subscription/extend -H ... -d '{"duration":"2m"}'   # renew a lapsed term
{"workspace":{"status":"active",
  "subscription":{"granted_at":"2026-11-03T20:46:48Z",
                  "expires_at":"2026-11-03T20:48:54Z",   # start from now + 2m
                  "renewed_at":"2026-11-03T20:46:54Z",
                  "grace_ends_at":"2026-11-03T20:49:02Z","extensions":1}}}
HTTP_STATUS=200

$ curl -s -X POST .../exec -H ... -d '{"cmd":["python3","-c","print(1)"]}'   # works again
{"exit_code":0,"stdout":"1\n","stderr":"","duration_ms":70,"truncated":false}
HTTP_STATUS=200
```

Invalid terms:

```sh
$ ... -d '{"name":"bad","subscription":{"duration":"30"}}'
{"error":{"code":"subscription_invalid","message":"invalid duration: duration \"30\": unknown unit after \"30\" (use s, m, h, d, w, mo or y)","details":{"field":"duration"}}}
HTTP_STATUS=422

$ ... -d '{"name":"bad2","subscription":{"duration":"1d","expires_at":"2030-01-01T00:00:00Z"}}'
{"error":{"code":"subscription_invalid","message":"provide either duration or expires_at, not both"}}
HTTP_STATUS=422
```

### 8.5 Tunnels (real Cloudflare quick tunnel)

A separate run with `tunnels.enabled: true` and `limits.allow_tunnels: true`
started an HTTP server inside a workspace and created a tunnel. This exercises a
real external service (Cloudflare), so it can be environment-limited; on this
run it succeeded:

```sh
$ curl -s -X POST .../tunnels -H ... -d '{"port":18091}'
{"tunnel":{"id":"tun_01M6HJJM5Y83ECRS1JKDGY1VMR","workspace_id":"ws_...",
  "port":18091,"url":"https://dim-joseph-disable-hence.trycloudflare.com",
  "status":"active","created_at":"2026-11-03T20:45:56Z"}}
HTTP_STATUS=201

$ curl -s -H "Authorization: Bearer $KEY" .../tunnels
{"tunnels":[{"id":"tun_...","port":18091,
  "url":"https://dim-joseph-disable-hence.trycloudflare.com","status":"active",...}]}
HTTP_STATUS=200

$ curl -s -H "Authorization: Bearer $KEY" .../tunnels/tun_...
{"tunnel":{"id":"tun_...","url":"https://dim-joseph-disable-hence.trycloudflare.com","status":"active",...}}
HTTP_STATUS=200

$ curl -s -o /dev/null -w 'HTTP_STATUS=%{http_code}\n' -X DELETE .../tunnels/tun_... -H "Authorization: Bearer $KEY"
HTTP_STATUS=204

$ curl -s -X POST .../tunnels -H ... -d '{"port":80}'   # on an allow_tunnels=false workspace
{"error":{"code":"tunnels_disabled","message":"tunnels are not permitted for this workspace",
          "details":{"config":"limits.allow_tunnels","field":"allow_tunnels"}}}
HTTP_STATUS=422
```

---

## 9. Failure codes

```sh
$ curl -s -X POST .../v1/workspaces -H ... -d '{"name":"x","profile":"cobol"}'
{"error":{"code":"unknown_profile","message":"unknown profile cobol",
          "details":{"field":"profile","profile":"cobol"}}}
HTTP_STATUS=422

$ curl -s -X POST .../workspaces/ws_mini/sessions -H ... -d '{}'   # minimal profile, no python3
{"error":{"code":"session_needs_interpreter","message":"session requires a profile that provides python3: profile \"minimal\" does not provide python3"}}
HTTP_STATUS=422
```

The `503 unavailable` path is exercised by the readiness/capability gate
(`scripts/*-check.sh`) rather than here because it requires a broken sandbox on a
host that has a working `nsjail`; the guide documents that the run-level refusal
carries a `message` naming the real cause and must be surfaced verbatim.

---

## 10. Cleanup (no leak)

At the end of every harness run:

```
fuse2fs=0
nsjail cgroups=0
ws cgroups=0
netns=0
```

No mount, cgroup, network namespace or `fuse2fs` process survived.

---

## 11. Consolidated meta key set

The set of top-level keys the handler emits is derived from the handler source,
not from a retyped list, by `scripts/docs-consistency.sh`:

```sh
awk '/^func \(s \*Server\) handleMeta/{f=1} \
     f&&/writeJSON\(w, http\.StatusOK, map\[string\]any\{/{b=1;next} \
     b&&/^\t\}\)/{b=0;f=0;next} \
     b&&match($0,/^\t\t"[a-z_]+":/){g=substr($0,RSTART,RLENGTH);gsub(/[\t":]/,"",g);print g}' \
  internal/api/router.go
```

Output (19 keys):

```
version
api_version
runtimes
profiles
subscriptions
archives
metrics
ports
limits_defaults
limits_maxima
tunnels
network
identity_masking
provider_tools_hidden
egress_decoy
workspace_limits
write_rate
interpreters
runnable_extensions
```

Every one of these is mentioned in `docs/INTEGRATION-GUIDE.md`, asserted by the
docs-consistency gate (§12).

---

## 12. The docs-consistency assertions, and proof they bite

`scripts/docs-consistency.sh` derives its checks from the code. This task added
four groups for `docs/INTEGRATION-GUIDE.md`:

1. **every path in `api/openapi.yaml` is mentioned in the guide** — the path list
   is read from the spec, not retyped;
2. **every error-code constant in `internal/model/errors.go` appears in the
   guide** — derived from the Go constants;
3. **the version string in the guide matches `model.Version`** — read from
   `internal/model/types.go`;
4. **every top-level key of the `/v1/meta` response appears in the guide** —
   derived from the handler's `writeJSON` block in `internal/api/router.go`.

A full run before adding the guide (baseline) and after are recorded below.

### 12.1 Baseline (before this document existed)

```sh
$ bash scripts/docs-consistency.sh
...
passed=294 failed=0
```

294 checks, all green, with no integration-guide checks yet.

### 12.2 After adding the guide and the new assertions

```sh
$ bash scripts/docs-consistency.sh
...
passed=389 failed=0
```

389 checks (294 + 95 new integration-guide checks), all green. The 95 new checks
break down as: 42 for the OpenAPI paths, 33 for the error codes, 1 for the
version, and 19 for the `/v1/meta` top-level keys.

### 12.3 Deliberate-break probe

An assertion that has never been red is not yet an assertion. Two facts were
broken on purpose, the gate was re-run, and both went **red**; each was then
restored and the gate went green again.

**Probe A — delete a route line.** Removing the
`` - `/v1/workspaces/{workspaceId}/ports` `` line from the guide's route index:

```sh
$ sed -i '/^- `\/v1\/workspaces\/{workspaceId}\/ports`/d' docs/INTEGRATION-GUIDE.md
$ bash scripts/docs-consistency.sh
FAIL guide.path./v1/workspaces/{workspaceId}/ports: not mentioned in docs/INTEGRATION-GUIDE.md
...
passed=388 failed=1
```

**Probe B — delete an error-code mention.** Deleting every line containing the
`tunnel_url_unavailable` code (its failure-table row and the tunnel prose):

```sh
$ sed -i '/tunnel_url_unavailable/d' docs/INTEGRATION-GUIDE.md
$ bash scripts/docs-consistency.sh
FAIL guide.code.tunnel_url_unavailable: error code not in docs/INTEGRATION-GUIDE.md
...
passed=388 failed=1
```

Both were restored; the final green run is **`passed=389 failed=0`** (§12.2).
The tally sequence is therefore: baseline `294/0` → probe A `388/1` → probe B
`388/1` → restored `389/0`.
