# 9init: A 9P-native Session Init System

## Motivation

The launchd agents that manage fontsrv, plumber, acme-lsp, acme-styles,
acme-treesitter, acme-hotkey, and acme-focused are unreliable. launchd's
`PathState` watch is poll-based: when a service crashes and its socket
vanishes and reappears inside launchd's polling window, dependents are never
cycled. The result is stale connections that require manual `launchctl
kickstart` to fix. As the service graph grows, the plist corpus becomes
harder to maintain.

9init replaces all of these agents with a single persistent session daemon
that owns process supervision, dependency ordering, and observability—all
through 9P, in the spirit of Plan 9.

---

## Scope

9init is **not** an acme-specific tool. It is a general-purpose session init
system for plan9port environments. It manages any long-running service that
participates in the plan9port namespace.

- It **starts** services it fully owns (fontsrv, plumber, acme-styles, etc.).
- It **watches** services it does not own (acme): when their namespace socket
  appears, dependents start; when it disappears, dependents stop.
- It exposes its own state as a 9P filesystem at `$NAMESPACE/init`.
- A companion binary `9init` is a plain 9P client for ad-hoc control.

---

## Plan 9 Inspiration

Plan 9's session init (`termrc`, `cpurc`) is a sequential rc script. Services
signal readiness by posting a file descriptor to `/srv`. Dependent services
open that `/srv` entry and mount the tree. The kernel's `/srv` is the session
registry: existence in it means "I am ready."

In plan9port, `/srv` becomes `$NAMESPACE/` — a per-session directory of Unix
sockets. The protocol is identical; the transport differs. Every service 9init
manages already uses this convention. **Socket appearance = readiness.** This
is 9init's only readiness probe.

Key principles carried forward:

| Plan 9 | 9init |
|--------|-------|
| Post to `/srv/name` | Create socket at `$NAMESPACE/name` |
| Remove from `/srv` on exit | Socket disappears on exit/crash |
| Sequential `termrc` with explicit order | Dependency graph; start in topo order |
| `ls /srv` to see what is running | `9p read init/status` |
| Kernel `NOTE_WRITE` on `/srv` | `kqueue EVFILT_VNODE` on `$NAMESPACE/` |

---

## Service Taxonomy

### Managed services
9init starts, monitors, and restarts these. They are owned by 9init.

### Watched services
9init does **not** start these. It watches `$NAMESPACE/` for their socket
to appear or disappear, and reacts by starting or stopping their dependents.
Acme is a watched service: it is started by the user (via the `a` script,
possibly under lldb), and 9init reacts to its presence.

---

## Full Service Graph

```
[9init starts at login]
│
├── fontsrv          (managed; no deps; posts $NAMESPACE/font)
├── plumber          (managed; no deps; posts $NAMESPACE/plumb)
│
└── [watches for $NAMESPACE/acme to appear]
    └── acme                          (watched; started externally by `a`)
        ├── acme-styles               (managed; after: acme → $NAMESPACE/acme-styles)
        │   ├── acme-treesitter       (managed; after: acme-styles)
        │   └── acme-lsp             (managed; after: acme, acme-styles)
        ├── acme-hotkey               (managed; after: acme → $NAMESPACE/acme-hotkey)
        │   └── acme-hotkeys          (managed; after: acme-hotkey; ready: started)
        └── acme-focused              (managed; after: acme)
```

When `$NAMESPACE/acme` disappears (acme exits or crashes), 9init stops all
managed dependents in reverse topological order. When `$NAMESPACE/acme`
reappears, it starts them again. 9init itself does not exit; it persists
for the lifetime of the login session.

---

## Config: A Folder of rc Scripts

Location: `$home/lib/9init/` — each `*.rc` file in this directory defines
one service. The service name is the filename minus the `.rc` extension.

Each file is a valid rc script with a TOML metadata block embedded in the
leading comment lines. Since `#` is the comment character in both rc and
TOML, the metadata is invisible to rc — 9init strips the `#` prefix from
leading comment lines and parses the result as TOML. The first blank line
ends the metadata block; everything after is the script body.

9init starts managed services by executing `rc /path/to/service.rc`. The
script body handles any setup (environment, directories, etc.) and ends with
`exec servicebinary args`, which replaces rc with the service process. This
makes the service binary the direct child of 9init, so `wait(2)` immediately
reports its exit and the `pid` file contains the right value. Without `exec`,
rc stays alive as the child and the binary is a grandchild — orphaned when
rc is killed.

### Example scripts

**`$home/lib/9init/fontsrv.rc`**
```rc
#!/usr/bin/env rc
# socket  = "font"
# restart = "on-failure"

exec fontsrv
```

**`$home/lib/9init/plumber.rc`**
```rc
#!/usr/bin/env rc
# socket  = "plumb"
# restart = "on-failure"

exec plumber -f
```

**`$home/lib/9init/acme.rc`** (watched; body never executed)
```rc
#!/usr/bin/env rc
# watch  = true
# socket = "acme"
# 9init watches $NAMESPACE/acme but never starts or restarts acme.
```

**`$home/lib/9init/acme-styles.rc`**
```rc
#!/usr/bin/env rc
# socket  = "acme-styles"
# after   = ["acme"]
# restart = "on-failure"

exec acme-styles -styles $home/lib/acme/styles -v
```

**`$home/lib/9init/acme-treesitter.rc`**
```rc
#!/usr/bin/env rc
# socket  = "acme-treesitter"
# after   = ["acme-styles"]
# restart = "on-failure"

exec acme-treesitter --config $home/lib/acme-treesitter/config.yaml
```

**`$home/lib/9init/acme-lsp.rc`**
```rc
#!/usr/bin/env rc
# socket  = "acme-lsp"
# after   = ["acme", "acme-styles"]
# restart = "on-failure"

ACME_LSP_CONFIG=$home/lib/acme-lsp/config.toml
exec acme-lsp
```

**`$home/lib/9init/acme-hotkey.rc`**
```rc
#!/usr/bin/env rc
# socket  = "acme-hotkey"
# after   = ["acme"]
# restart = "on-failure"

exec acme-hotkey
```

**`$home/lib/9init/acme-hotkeys.rc`**
```rc
#!/usr/bin/env rc
# after   = ["acme-hotkey"]
# ready   = "started"
# restart = "always"

# Pipeline: rc stays alive as process group leader.
# 9init kills the whole process group on stop.
printf 'devdraw\n' | 9p write acme-hotkey/filter &&
9p read acme-hotkey/keys |
cawk -f $home/src/acme-hotkey/hotkeys.cawk |
9p write acme-hotkey/keys
```

**`$home/lib/9init/acme-focused.rc`**
```rc
#!/usr/bin/env rc
# socket  = "acme-focused"
# after   = ["acme"]
# restart = "on-failure"

exec acme-focused
```

### Why this works well

- **Pre-start setup is free.** Any rc commands before the final `exec` act as
  `ExecStartPre` hooks (create directories, clean up stale sockets, set
  environment, etc.) — no separate hook mechanism needed.
- **Environment is natural.** `ACME_LSP_CONFIG=$home/lib/acme-lsp/config.toml`
  in the script body is cleaner than a TOML `env` table.
- **Scripts are self-contained.** Adding a new service is dropping a file.
  Removing one is deleting it. No central config file to edit.
- **Scripts are directly executable.** `rc $home/lib/9init/acme-styles.rc`
  runs the service by hand for debugging without involving 9init at all.
- **Pipeline services work naturally.** acme-hotkeys no longer needs a wrapper
  script in `~/bin/`; the pipeline is written directly in the service file.

### Metadata fields

| field | required | description |
|-------|----------|-------------|
| `socket` | yes, unless `ready = "started"` | basename of the Unix socket posted to `$NAMESPACE/` |
| `watch` | no (default false) | if true, 9init watches only; body is never executed |
| `after` | no (default []) | service names that must be ready before this starts |
| `ready` | no (default `"socket"`) | `"socket"`: wait for socket; `"started"`: ready on exec |
| `restart` | no (default `"on-failure"`) | `"on-failure"`, `"always"`, `"never"` |
| `timeout` | no (default `"30s"`) | start timeout before treating non-appearance of socket as crash |

`socket` is always explicit; it is never inferred from the filename.
Config validation fails at startup if any `ready = "socket"` service omits
`socket`.

### Frontmatter parsing

9init reads each `*.rc` file and collects the contiguous block of lines
beginning with `#` that immediately follows the optional shebang. It strips
the leading `# ` prefix and parses the result as TOML. The first blank line
or first non-comment line ends the block. Lines containing only `#` (bare
comment lines) are passed through as blank lines, which TOML ignores.

Because TOML is also `#`-commented, the metadata block is simultaneously
valid rc (ignored as comments) and valid TOML (parsed by 9init).

---

## Startup Sequence

```
1. 9init scans $home/lib/9init/*.rc; parses frontmatter from each file;
   validates graph (cycle detection, unknown deps, missing socket fields).
2. 9init opens $NAMESPACE/ with kqueue EVFILT_VNODE (NOTE_WRITE) to receive
   kernel notifications of directory changes—no polling.
3. 9init posts its own 9P filesystem at $NAMESPACE/init via 9pserve
   (same mechanism as acme-styles and all other plan9port 9P servers).
4. 9init starts all managed services with no dependencies (fontsrv, plumber)
   by running `rc /path/to/service.rc` for each, concurrently.
5. kqueue fires when $NAMESPACE/font and $NAMESPACE/plumb appear.
   9init marks each service "ready".
6. No further startup happens until $NAMESPACE/acme appears (externally,
   when the user runs `a`).
7. On $NAMESPACE/acme appearing: 9init starts acme's direct dependents
   (acme-styles, acme-hotkey, acme-focused) concurrently.
8. As each socket appears, the next tier starts (acme-treesitter, acme-lsp,
   acme-hotkeys).
```

---

## Crash and Restart

### Managed service crashes

1. 9init's `wait(2)` loop detects the exit immediately (9init is the direct
   parent of every managed process).
2. All managed services that transitively depend on the crashed service are
   stopped: SIGTERM, then SIGKILL after 5 s.
3. The crashed service restarts (subject to backoff).
4. Once it posts its socket, dependents restart in topological order.

**Backoff:** Exponential, starting at 1 s, doubling, capped at 30 s, with
jitter. A service that ran for ≥ 10 s resets its backoff counter.

### Watched service disappears (acme exits or crashes)

1. kqueue fires on NOTE_WRITE; stat reveals `$NAMESPACE/acme` is gone.
2. All managed dependents are stopped in reverse topological order.
3. State is recorded. 9init waits silently for `$NAMESPACE/acme` to
   reappear.
4. When it does, the full dependency chain starts again.

9init does **not** distinguish a clean acme exit from a crash. Either way,
dependents stop and will restart when acme does. The user decides whether to
re-run `a`.

---

## 9P Filesystem: `$NAMESPACE/init`

9init serves a 9P filesystem using the `srv9p` package from `9fans.net/go`,
posted via a `9pserve unix!$NAMESPACE/init` socketpair (the same mechanism
as acme-styles). It is accessible to any plan9port tool:

```sh
9p read init/status
9p write init/ctl 'restart acme-lsp'
9p read init/svc/acme-styles/log
```

### Filesystem layout

```
init/
├── ctl                      # write-only: global control commands
├── status                   # read-only: one line per service, tab-separated
└── svc/
    ├── fontsrv/
    │   ├── status           # ready | starting | stopped | crashed | watching
    │   ├── pid              # current PID, empty if not running
    │   ├── uptime           # seconds since last start, "-" if not running
    │   ├── restarts         # restart count this session
    │   └── log              # streaming: reads block for new lines (like tail -f)
    ├── plumber/
    │   └── ...
    ├── acme/
    │   └── ...              # acme's status is "watching" or "ready"; no pid/uptime
    └── acme-styles/
        └── ...
```

### `init/ctl` commands

Written as a newline-terminated line:

```
start <service>          start a stopped managed service (and its ready deps)
stop <service>           graceful stop (SIGTERM → SIGKILL after 5 s)
restart <service>        stop + start
kill <service>           immediate SIGKILL
shutdown                 stop all managed services; 9init exits
```

### `init/status` format

```
fontsrv      ready    pid=3935  uptime=3600s  restarts=0
plumber      ready    pid=3928  uptime=3600s  restarts=0
acme         ready    pid=-     uptime=-      restarts=0
acme-styles  ready    pid=4012  uptime=120s   restarts=1
acme-lsp     starting pid=-     uptime=-      restarts=0
```

### `svc/<name>/log` (streaming)

Reads block until new data arrives, enabling `9p read init/svc/acme-lsp/log`
as a live stream. 9init captures stdout and stderr from each child, prepending
a timestamp and stream tag per line:

```
2026-03-01T15:30:00Z out acme-styles: loaded 12 palette entries
2026-03-01T15:30:01Z err acme-lsp: connecting to gopls...
```

---

## Log Persistence and Rotation

9init writes each service's log to disk. A dedicated `logwriter` package
within the 9init repo handles file management, keeping the supervision core
clean. The `logwriter` is the single component responsible for:

- Opening `$home/Library/Logs/9init/<service>.log` on first output.
- Creating parent directories automatically.
- **Rotation:** when the file exceeds a configurable size threshold
  (default 10 MiB), it is renamed to `<service>.log.1`, shifting
  `log.1` → `log.2` and so on, keeping the last N files (default 5).
  The new `<service>.log` is opened fresh. Rotation is done atomically
  within the `logwriter` goroutine with no lock visible to the caller.
- Exposing the in-memory tail (last 8 KiB) for the streaming
  `svc/<name>/log` 9P file without a disk read.

`logwriter` has no external dependencies and could be published as a
standalone package if useful to other tools in the acme ecosystem.

The log directory layout mirrors what launchd currently writes, making the
transition transparent to any existing log-reading scripts:

```
$home/Library/Logs/9init/
├── fontsrv.log
├── fontsrv.log.1
├── plumber.log
├── acme-styles.log
└── acme-lsp.log
```

---

## The `9init` CLI Binary

A companion binary `9init` (placed in `~/go/bin` or `~/bin`) is a thin 9P
client against `$NAMESPACE/init`. It is not a separate daemon; it exits
after each operation.

```sh
9init status                  # print init/status
9init start acme-lsp          # write 'start acme-lsp' to init/ctl
9init stop acme-lsp
9init restart acme-lsp
9init log acme-styles         # stream init/svc/acme-styles/log (like tail -f)
9init log -n 50 acme-lsp     # print last 50 lines then stream
9init shutdown
```

All of these are equivalent to the corresponding `9p read`/`9p write`
invocations; the binary just provides tab completion, flag parsing, and
friendlier output formatting.

---

## The `a` Script

The `a` script is **unchanged**. 9init runs as a separate session daemon;
it does not start acme. The user types `a` exactly as today:

```rc
#!/usr/bin/env rc
. $home/lib/profile
TERM=dumb
lldb -- acme -a -f /mnt/font/Iosevka/12a/font -s $home/lib/acme/styles $*
```

When acme posts `$NAMESPACE/acme`, 9init's kqueue watcher fires and the
acme-dependent services come up automatically.

---

## launchd Migration

Replace all seven existing acme/fontsrv/plumber agents with a single agent:

```xml
<!-- $home/Library/LaunchAgents/zip.connor.9init.plist -->
<key>Label</key><string>zip.connor.9init</string>
<key>ProgramArguments</key>
<array>
  <string>/Users/cptaffe/go/bin/9init</string>
  <string>-services</string>
  <string>/Users/cptaffe/lib/9init</string>
</array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
```

The `KeepAlive=true` here is intentional: if 9init itself crashes (a bug),
launchd restarts it. fontsrv and plumber then restart under 9init's
supervision. The seven existing plists are deleted.

---

## What launchd and systemd Have That We're Missing

Going through the full feature set of both systems and making an explicit
decision for each, since the answer for some will change how we implement
the supervisor.

### Process environment — gaps the rc script body already fills

| feature | launchd/systemd | 9init |
|---------|----------------|-------|
| Pre-start commands | `ExecStartPre=` | rc lines before final `exec` |
| Post-stop cleanup | `ExecStopPost=` | not needed: rc exits when service does |
| Working directory | `WorkingDirectory=` | `cd /some/dir` before `exec` |
| umask | `Umask=` | `umask 022` before `exec` |
| Extra environment | `Environment=` / `EnvironmentFile=` | variable assignments before `exec` |

The rc script body is a full shell; none of these need to be metadata fields.

### Process isolation — gaps 9init must fill explicitly

**Process groups.** 9init must call `setpgid(0, 0)` on each child
immediately after fork, placing it in its own process group. Two reasons:

1. **Signal isolation.** If 9init is started from a terminal and the user
   sends Ctrl-C, SIGINT is delivered to the entire foreground process group.
   Without `setpgid`, every managed service gets SIGINT too.
2. **Clean teardown of pipelines.** acme-hotkeys is a shell pipeline: rc
   spawns `9p read`, `cawk`, and `9p write` as children. Killing only rc's
   PID orphans them. Killing `-pgid` (the process group) takes out the
   entire pipeline atomically. This must be 9init's default kill strategy:
   `kill(-pgid, SIGTERM)` followed by `kill(-pgid, SIGKILL)`.

**stdin.** Managed services inherit 9init's stdin (a launchd pipe or
terminal). They should have stdin connected to `/dev/null` by default; the
service scripts can override this if needed (`</dev/null` in rc before exec).
9init will open `/dev/null` and dup it onto fd 0 of each child.

### Crash rate limiting — gap 9init must fill

launchd has `ThrottleInterval` (default 10 s minimum between restarts).
systemd has `StartLimitBurst` and `StartLimitIntervalSec`: if a service
crashes more than N times in M seconds, it enters a "failed" state and
stops being restarted until manually reset.

The current plan has exponential backoff but no ceiling on attempts.
Without a give-up policy, a service that crashes on every start (e.g.,
due to a misconfiguration introduced while 9init is running) will loop
indefinitely, filling logs.

**Decision:** Add a crash budget. If a service exits within `min_runtime`
of starting (default 5 s) more than `max_restarts` times (default 5) within
a rolling window of `restart_window` (default 60 s), 9init marks it
`failed` and stops restarting. A `start` command via `init/ctl` clears the
budget and tries again. These are configurable per-service as frontmatter
fields: `max_restarts`, `restart_window`, `min_runtime`.

### Stop timeout — minor gap, worth being explicit

launchd and systemd have separate start and stop timeouts. The current
`timeout` field covers only the start (how long to wait for a socket to
appear). A separate `stop_timeout` (how long to wait between SIGTERM and
SIGKILL during shutdown) defaults to 5 s but should be overridable — some
services (acme-lsp, talking to live language servers) may need more time.
Add `stop_timeout` as an optional frontmatter field.

### Reload signal — small gap, worth supporting

systemd has `ExecReload=` and `kill -s HUP`. Many unix services (nginx,
sshd) reload config on SIGHUP without restarting. Add a `reload_signal`
frontmatter field (default: none; typical value: `"HUP"`), surfaced as:

```
9init reload acme-lsp
```

Which writes `reload acme-lsp` to `init/ctl`, causing 9init to send the
configured signal to the service's process group.

### Features explicitly out of scope

| feature | reason omitted |
|---------|---------------|
| Socket activation | 9P services create their own sockets; no benefit |
| Cgroup resource limits | Linux-only; not relevant on macOS |
| Filesystem sandboxing | Linux-only (`ProtectSystem`, namespaces) |
| D-Bus activation | Not in this ecosystem |
| Service templates (`svc@name`) | Not needed at current scale |
| Watchdog / sd_notify | Services are supervised via socket appearance; sufficient |
| User/group switching | 9init runs as the login user; all services do too |
| Timer/calendar units | Use cron or launchd for scheduled tasks |

### Updated metadata fields

| field | default | description |
|-------|---------|-------------|
| `socket` | — | required (unless `ready = "started"`): socket basename in `$NAMESPACE/` |
| `watch` | `false` | if true, never started; 9init only watches the socket |
| `after` | `[]` | services that must be ready before this one starts |
| `ready` | `"socket"` | `"socket"` or `"started"` |
| `restart` | `"on-failure"` | `"on-failure"`, `"always"`, `"never"` |
| `timeout` | `"30s"` | time to wait for socket to appear before treating as crash |
| `stop_timeout` | `"5s"` | time between SIGTERM and SIGKILL on stop |
| `reload_signal` | `""` | signal name to send on `9init reload` (e.g. `"HUP"`) |
| `max_restarts` | `5` | crash budget: max exits within `restart_window` before `failed` |
| `restart_window` | `"60s"` | rolling window for crash budget |
| `min_runtime` | `"5s"` | exits faster than this count against the crash budget |

---

## Updated Service State Machine

```
          ┌──────────────────────────────────────────────┐
          │                                              │
     ┌────▼─────┐   exec ok    ┌──────────┐             │
     │ stopped  ├─────────────►│ starting │             │
     └──────────┘              └────┬─────┘             │
          ▲                         │                    │
          │            socket       │                    │
          │            appears      ▼                    │
          │                    ┌───────┐                 │
          │    socket    ◄─────│ ready │                 │
          │    disappears      └───┬───┘                 │
          │                       │                      │
          │               process │ exits                │
          │                       ▼                      │
          │                  ┌─────────┐  budget ok      │
          │                  │ crashed ├────────────────►│
          │                  └────┬────┘                 │
          │                       │ budget               │
          │                       │ exhausted            │
          │                  ┌────▼────┐                 │
          └──────────────────┤ failed  │ (manual reset)  │
                             └─────────┘

Watched services cycle only between "watching" and "ready".
```

---

## Implementation Plan

### Repository: `~/src/9init`

```
9init/
├── cmd/
│   ├── 9init/          # daemon
│   └── 9init/ctl/      # CLI client subcommand
├── internal/
│   ├── config/         # *.rc frontmatter parsing, graph validation
│   ├── graph/          # topological sort, transitive dep computation
│   ├── supervisor/     # process start/stop/wait, backoff, state machine
│   ├── watcher/        # kqueue EVFILT_VNODE on $NAMESPACE/
│   ├── fs9p/           # srv9p-based 9P filesystem (status, ctl, log files)
│   └── logwriter/      # log file management and rotation
├── go.mod              # module github.com/cptaffe/9init
└── go.sum
```

### Dependencies (all already familiar)

| package | use |
|---------|-----|
| `9fans.net/go/plan9/srv9p` | 9P server for `$NAMESPACE/init` |
| `9fans.net/go/plan9/client` | `client.Namespace()` for `$NAMESPACE` path |
| `golang.org/x/sys/unix` | kqueue, socketpair, `CloseOnExec` |
| `github.com/BurntSushi/toml` | frontmatter parsing from `*.rc` files |

### Milestones

1. **`internal/config`** — scan `*.rc` files; extract leading `# …` frontmatter
   block; parse as TOML; validate schema (required fields, unknown keys);
   cycle detection in the dependency graph.
2. **`internal/graph`** — topological sort; transitive dependent computation
   (needed for "stop all dependents of crashed service").
3. **`internal/watcher`** — kqueue `EVFILT_VNODE` on `$NAMESPACE/`, emits
   `{name, created|deleted}` events; unit-testable with a temp dir.
4. **`internal/supervisor`** — per-service state machine
   (`stopped → starting → ready → stopped/crashed`), exec, stdout/stderr
   capture, `wait(2)` loop, backoff, SIGTERM/SIGKILL teardown.
5. **`internal/logwriter`** — io.Writer implementation that fans out to the
   in-memory ring (for streaming 9P reads) and the rotating log file.
6. **`internal/fs9p`** — srv9p Tree for `init/`, wired to supervisor state.
   Streaming `log` files use a broadcast channel; reads block until new data.
   `ctl` writes dispatch to supervisor.
7. **Daemon wiring** (`cmd/9init`) — tie watcher events to supervisor
   transitions; wire supervisor state changes to fs9p; handle `shutdown`.
8. **`cmd/9init-ctl`** — CLI client; `9p` dial against `$NAMESPACE/init`.
9. **launchd plist** — single `zip.connor.9init.plist`; delete the seven
   existing plists.

---

## Decisions

| question | decision |
|----------|----------|
| lldb wrapping | `a` unchanged; 9init watches, not starts acme; lldb is transparent |
| root crash policy | acme is watched; 9init never starts or restarts it; user decides |
| acme-hotkeys | inline pipeline in `acme-hotkeys.rc`; `restart = "always"` |
| config format | folder of rc scripts; TOML frontmatter in leading `#` comments |
| socket field | always explicit; never inferred from service/filename |
| 9P mount | `$NAMESPACE/init` via 9pserve socketpair; no FUSE needed |
| log rotation | `logwriter` package inside 9init repo; size-based, N-file retention |
| pre-acme services | fontsrv, plumber fully managed by 9init; no acme dep |
| CLI | `9init` binary subcommands; thin 9P client |
| process groups | `setpgid(0,0)` on each child; all kills via `kill(-pgid, sig)` |
| stdin | `/dev/null` by default; scripts override with `</dev/null` in rc |
| crash looping | crash budget: `max_restarts` in `restart_window`; → `failed` state |
| stop timeout | separate `stop_timeout` field; default 5 s |
| config reload | `reload_signal` frontmatter field; `9init reload <svc>` command |
