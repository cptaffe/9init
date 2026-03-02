// Package supervisor manages the lifecycle of all configured services.
//
// A single goroutine (the event loop) owns all state transitions. External
// callers interact through the Control method and thread-safe Snapshot reads.
// Child processes run in their own process groups (setpgid) so that teardown
// signals hit every process in a pipeline, not just the rc parent.
package supervisor

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cptaffe/9init/internal/config"
	"github.com/cptaffe/9init/internal/graph"
	"github.com/cptaffe/9init/internal/logwriter"
	"github.com/cptaffe/9init/internal/watcher"
)

// State is the lifecycle state of a service.
type State int

const (
	StateStopped  State = iota // not running; clean stop or initial state
	StateStarting              // exec'd; waiting for socket or "started" ready
	StateReady                 // socket present; dependents may start
	StateCrashed               // exited unexpectedly; backoff retry pending
	StateFailed                // crash budget exhausted; manual restart required
	StateWatching              // watched service; socket not yet present
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateCrashed:
		return "crashed"
	case StateFailed:
		return "failed"
	case StateWatching:
		return "watching"
	default:
		return "unknown"
	}
}

// Snapshot is an immutable view of a service's current state, safe to read
// outside the event loop.
type Snapshot struct {
	Name     string
	State    State
	Pid      int       // 0 if not running
	Started  time.Time // zero if not running
	Restarts int
}

// serviceState is the mutable per-service state owned by the event loop.
type serviceState struct {
	cfg      *config.Service
	state    State
	cmd      *exec.Cmd
	pgid     int       // process group id; equals cmd.Process.Pid after Start
	started  time.Time // when the current process was exec'd
	restarts int

	// crash budget
	recentCrashes []time.Time // timestamps of recent rapid exits
}

// event kinds sent to the supervisor's event loop.
// Watcher events (socket appeared/disappeared) are handled inline in Run
// rather than via this channel, so they are not listed here.
type eventKind int

const (
	evProcessExited eventKind = iota // wait goroutine: child exited
	evRetry                          // backoff timer or restart cmd: start if not running
	evCtl                            // control command from fs9p or CLI
)

type event struct {
	kind eventKind

	// evSocketAppeared / evSocketDisappeared
	socketName string

	// evProcessExited
	procName   string
	exitStatus int
	exitSignal bool

	// evCtl
	ctlCmd string
	reply  chan error
}

// Supervisor manages service processes and reacts to namespace socket events.
type Supervisor struct {
	g      *graph.Graph
	ns     string // $NAMESPACE path
	logDir string

	// socketIndex maps socket basename → service name for O(1) lookup.
	socketIndex map[string]string

	events chan event

	// states is protected by mu for reads from outside the event loop.
	mu     sync.RWMutex
	states map[string]*serviceState

	loggers map[string]*logwriter.Writer

	// watcher is stored at Run() time so startService can call Forget
	// before each exec, clearing any stale socket from the watcher's
	// existing-set so the new socket fires a genuine Exists=true event.
	watcher interface{ Forget(string) }
}

// New creates a Supervisor for the given graph.
// ns is the plan9port namespace directory (from client.Namespace()).
// logDir is where rotating log files are written.
func New(g *graph.Graph, ns, logDir string) (*Supervisor, error) {
	s := &Supervisor{
		g:           g,
		ns:          ns,
		logDir:      logDir,
		socketIndex: make(map[string]string),
		events:      make(chan event, 128),
		states:      make(map[string]*serviceState),
		loggers:     make(map[string]*logwriter.Writer),
	}

	for _, svc := range g.Order() {
		st := &serviceState{cfg: svc}
		if svc.Watch {
			st.state = StateWatching
		} else {
			st.state = StateStopped
		}
		s.states[svc.Name] = st

		if svc.Socket != "" {
			s.socketIndex[svc.Socket] = svc.Name
		}

		if !svc.Watch {
			lw, err := logwriter.New(
				filepath.Join(logDir, svc.Name+".log"),
				10*1024*1024, // 10 MiB
				5,
			)
			if err != nil {
				return nil, fmt.Errorf("logwriter for %s: %w", svc.Name, err)
			}
			s.loggers[svc.Name] = lw
		}
	}

	return s, nil
}

// LogWriter returns the log writer for the named service, or nil.
func (s *Supervisor) LogWriter(name string) *logwriter.Writer {
	return s.loggers[name]
}

// Snapshot returns an immutable state snapshot for the named service.
// ok is false if the service does not exist.
func (s *Supervisor) Snapshot(name string) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.states[name]
	if !ok {
		return Snapshot{}, false
	}
	snap := Snapshot{
		Name:     name,
		State:    st.state,
		Restarts: st.restarts,
	}
	if st.cmd != nil && st.cmd.Process != nil {
		snap.Pid = st.cmd.Process.Pid
		snap.Started = st.started
	}
	return snap, true
}

// AllSnapshots returns snapshots for all services in topological order.
func (s *Supervisor) AllSnapshots() []Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Snapshot, 0, len(s.states))
	for _, svc := range s.g.Order() {
		st := s.states[svc.Name]
		snap := Snapshot{
			Name:     svc.Name,
			State:    st.state,
			Restarts: st.restarts,
		}
		if st.cmd != nil && st.cmd.Process != nil {
			snap.Pid = st.cmd.Process.Pid
			snap.Started = st.started
		}
		out = append(out, snap)
	}
	return out
}

// Control sends a control command to the event loop and waits for the result.
// Supported commands: "start <name>", "stop <name>", "restart <name>",
// "reload <name>", "kill <name>", "shutdown".
func (s *Supervisor) Control(cmd string) error {
	reply := make(chan error, 1)
	s.events <- event{kind: evCtl, ctlCmd: cmd, reply: reply}
	return <-reply
}

// Run starts the event loop. It watches for namespace socket events from w
// and processes service lifecycle transitions until ctx is cancelled or a
// "shutdown" control command is received.
func (s *Supervisor) Run(ctx context.Context, w *watcher.Watcher) error {
	s.watcher = w

	// Bootstrap: start services that have no dependencies.
	for _, svc := range s.g.Order() {
		if svc.Watch {
			continue
		}
		if len(svc.After) == 0 {
			s.startService(svc.Name)
		}
	}

	// Pre-seed watched services from sockets that already exist in the namespace.
	// A watched service (e.g. acme) may have been started before 9init; its
	// socket is live and we should treat it as ready immediately.
	//
	// Managed services are NOT pre-seeded: an existing socket for a managed
	// service belongs to a process 9init doesn't own, so we can't supervise it.
	// We start managed services fresh; plan9port handles stale socket files by
	// removing them on EADDRINUSE before re-binding.
	for socketName := range w.Snapshot() {
		if name, ok := s.socketIndex[socketName]; ok {
			svc := s.g.Service(name)
			if svc != nil && svc.Watch {
				s.mu.Lock()
				st := s.states[name]
				if st.state == StateWatching {
					st.state = StateReady
					log.Printf("supervisor: %s already ready (socket exists at startup)", name)
				}
				s.mu.Unlock()
			}
		}
	}
	// Start dependents of any watched services already marked ready.
	s.startReady()

	// s.events carries events from background goroutines (wait loops, backoff
	// timers, ctl commands). Watcher events arrive on a separate channel and
	// are handled inline to avoid the deadlock that would occur if we tried
	// to forward them into s.events while s.events was full.
	for {
		select {
		case <-ctx.Done():
			s.shutdownAll()
			return ctx.Err()

		case wev, ok := <-w.Events():
			if !ok {
				return nil
			}
			if wev.Exists {
				s.onSocketAppeared(wev.Name)
			} else {
				s.onSocketDisappeared(wev.Name)
			}

		case ev := <-s.events:
			switch ev.kind {
			case evProcessExited:
				s.onProcessExited(ev.procName, ev.exitStatus, ev.exitSignal)
			case evRetry:
				s.onRetry(ev.procName)
			case evCtl:
				ev.reply <- s.onCtl(ev.ctlCmd)
			}
		}
	}
}

// ---- event handlers (called from event loop goroutine only) ----

func (s *Supervisor) onSocketAppeared(socketName string) {
	name, ok := s.socketIndex[socketName]
	if !ok {
		return
	}
	s.mu.Lock()
	st := s.states[name]
	if st.state == StateStarting || st.state == StateWatching {
		st.state = StateReady
	}
	s.mu.Unlock()
	log.Printf("supervisor: %s ready", name)
	s.startReady()
}

func (s *Supervisor) onSocketDisappeared(socketName string) {
	name, ok := s.socketIndex[socketName]
	if !ok {
		return
	}
	s.mu.Lock()
	st := s.states[name]
	prev := st.state
	if prev == StateReady {
		if st.cfg.Watch {
			st.state = StateWatching
		}
		// For managed services, the socket disappears because the process
		// exited; the evProcessExited event will handle the state transition.
	}
	s.mu.Unlock()

	if prev == StateReady {
		log.Printf("supervisor: %s socket gone", name)
		s.stopDependents(name)
	}
}

func (s *Supervisor) onProcessExited(name string, exitStatus int, bySignal bool) {
	s.mu.Lock()
	st := s.states[name]
	svc := st.cfg

	runtime := time.Since(st.started)
	wasRunning := st.cmd != nil
	st.cmd = nil
	st.pgid = 0

	if !wasRunning {
		s.mu.Unlock()
		return
	}

	success := exitStatus == 0 && !bySignal

	// Determine whether to restart.
	shouldRestart := false
	switch svc.Restart {
	case config.RestartAlways:
		shouldRestart = true
	case config.RestartOnFailure:
		shouldRestart = !success
	case config.RestartNever:
		shouldRestart = false
	}

	var nextState State
	var backoff time.Duration

	if !shouldRestart {
		nextState = StateStopped
	} else {
		// Check crash budget: count recent rapid exits.
		if runtime < svc.MinRuntime {
			now := time.Now()
			cutoff := now.Add(-svc.RestartWindow)
			// Drop old entries.
			fresh := st.recentCrashes[:0]
			for _, t := range st.recentCrashes {
				if t.After(cutoff) {
					fresh = append(fresh, t)
				}
			}
			fresh = append(fresh, now)
			st.recentCrashes = fresh

			if len(fresh) > svc.MaxRestarts {
				nextState = StateFailed
				log.Printf("supervisor: %s failed (crash budget exhausted after %d rapid exits)", name, len(fresh))
			} else {
				nextState = StateCrashed
				// Exponential backoff based on budget usage.
				backoff = time.Duration(1<<uint(len(fresh)-1)) * time.Second
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		} else {
			// Ran long enough; clear the budget and restart immediately.
			st.recentCrashes = st.recentCrashes[:0]
			nextState = StateCrashed
			backoff = 0
		}
	}

	st.state = nextState
	s.mu.Unlock()

	if success {
		log.Printf("supervisor: %s exited cleanly (runtime %s)", name, runtime.Round(time.Second))
	} else if bySignal {
		log.Printf("supervisor: %s killed by signal (runtime %s)", name, runtime.Round(time.Second))
	} else {
		log.Printf("supervisor: %s exited status %d (runtime %s)", name, exitStatus, runtime.Round(time.Second))
	}

	s.stopDependents(name)

	if nextState == StateCrashed {
		if backoff == 0 {
			s.startService(name)
		} else {
			log.Printf("supervisor: %s restarting in %s", name, backoff)
			time.AfterFunc(backoff, func() {
				s.events <- event{kind: evRetry, procName: name}
			})
		}
	}
}

func (s *Supervisor) onRetry(name string) {
	s.mu.RLock()
	state := s.states[name].state
	s.mu.RUnlock()
	if retryable(state) {
		s.startService(name)
	}
}

// retryable reports whether a service in state s should be started by onRetry.
//
// StateStopped is included so that a "restart" control command works on a
// service that is already stopped (exited cleanly or manually stopped) — not
// just on one that is in StateCrashed after an unexpected exit.
//
// StateFailed is excluded: it requires an explicit "start" command that first
// clears the crash budget via clearFailed.
// StateWatching is excluded: watched services are never started by 9init.
func retryable(s State) bool {
	return s == StateCrashed || s == StateStopped
}

func (s *Supervisor) onCtl(cmd string) error {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return fmt.Errorf("empty control command")
	}
	verb := parts[0]

	if verb == "shutdown" {
		s.shutdownAll()
		return nil
	}

	if len(parts) < 2 {
		return fmt.Errorf("usage: %s <service>", verb)
	}
	name := parts[1]
	if s.g.Service(name) == nil {
		return fmt.Errorf("unknown service %q", name)
	}

	switch verb {
	case "start":
		s.clearFailed(name)
		s.startService(name)
		return nil
	case "stop":
		s.stopService(name, false)
		return nil
	case "kill":
		s.stopService(name, true)
		return nil
	case "restart":
		s.stopService(name, false)
		s.clearFailed(name)
		// startService will be called once the process exits and dependents settle.
		// For simplicity, wait a beat then start.
		time.AfterFunc(s.g.Service(name).StopTimeout+200*time.Millisecond, func() {
			s.events <- event{kind: evRetry, procName: name}
		})
		return nil
	case "reload":
		return s.reloadService(name)
	default:
		return fmt.Errorf("unknown command %q", verb)
	}
}

// ---- service lifecycle helpers ----

// startService execs the service script and sets up the wait goroutine.
// It does nothing if the service is watched, already running, or failed.
func (s *Supervisor) startService(name string) {
	s.mu.Lock()
	st := s.states[name]
	svc := st.cfg

	if svc.Watch {
		s.mu.Unlock()
		return
	}
	switch st.state {
	case StateStarting, StateReady:
		s.mu.Unlock()
		return // already running
	case StateFailed:
		s.mu.Unlock()
		return // needs manual reset via "start"
	}

	// Check that all dependencies are ready.
	for _, dep := range svc.After {
		depSt := s.states[dep]
		if depSt.state != StateReady {
			s.mu.Unlock()
			return // dependency not ready yet
		}
	}

	// Before exec, remove any stale socket file and tell the watcher to
	// forget it. This must happen on every start, not just at initial
	// boot: when a service is killed (SIGKILL), its socket file is not
	// removed (Go's net.Listener cleanup can't run). The stale file would
	// cause the service to fail with EADDRINUSE on its next start, and
	// would prevent the watcher from emitting an Exists=true event (because
	// the socket was already in its existing-set from the previous run).
	if svc.Ready == config.ReadySocket && svc.Socket != "" {
		socketPath := filepath.Join(s.ns, svc.Socket)
		if err := os.Remove(socketPath); err == nil {
			log.Printf("supervisor: removed stale socket for %s", name)
		}
		if s.watcher != nil {
			s.watcher.Forget(svc.Socket)
		}
	}

	cmd := exec.Command("rc", svc.Path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// stdin → /dev/null; rc has no need for terminal input.
	cmd.Stdin = nil

	lw := s.loggers[name]
	cmd.Stdout = lw
	cmd.Stderr = lw

	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		log.Printf("supervisor: start %s: %v", name, err)
		return
	}

	st.cmd = cmd
	st.pgid = cmd.Process.Pid // setpgid: pgid == pid of the new process
	st.started = time.Now()
	st.restarts++
	if svc.Ready == config.ReadyStarted {
		st.state = StateReady
	} else {
		st.state = StateStarting
	}
	s.mu.Unlock()

	log.Printf("supervisor: started %s (pid %d)", name, cmd.Process.Pid)

	// Set up a start-timeout for socket-ready services.
	if svc.Ready == config.ReadySocket {
		time.AfterFunc(svc.Timeout, func() {
			s.mu.RLock()
			stillStarting := s.states[name].state == StateStarting &&
				s.states[name].cmd == cmd
			s.mu.RUnlock()
			if stillStarting {
				log.Printf("supervisor: %s start timeout; killing", name)
				s.killPgid(st.pgid, false)
			}
		})
	}

	// If ready=started, start any newly-unblocked dependents now.
	if svc.Ready == config.ReadyStarted {
		s.startReady()
	}

	// Wait goroutine: report exit to event loop.
	go func() {
		err := cmd.Wait()
		exitStatus := 0
		bySignal := false
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
					if ws.Signaled() {
						bySignal = true
					} else {
						exitStatus = ws.ExitStatus()
					}
				}
			}
		}
		s.events <- event{
			kind:       evProcessExited,
			procName:   name,
			exitStatus: exitStatus,
			exitSignal: bySignal,
		}
	}()
}

// startReady starts all managed services whose dependencies are now all ready.
func (s *Supervisor) startReady() {
	for _, svc := range s.g.Order() {
		if svc.Watch {
			continue
		}
		s.startService(svc.Name)
	}
}

// stopService sends SIGTERM to the service's process group, then SIGKILL after
// StopTimeout. immediate=true skips straight to SIGKILL.
func (s *Supervisor) stopService(name string, immediate bool) {
	s.mu.RLock()
	st := s.states[name]
	pgid := st.pgid
	svc := st.cfg
	s.mu.RUnlock()

	if pgid == 0 {
		return
	}
	s.killPgid(pgid, immediate)
	if !immediate {
		time.AfterFunc(svc.StopTimeout, func() {
			s.killPgid(pgid, true)
		})
	}

	s.mu.Lock()
	if s.states[name].pgid == pgid {
		s.states[name].state = StateStopped
	}
	s.mu.Unlock()
}

// stopDependents stops all managed services that transitively depend on name,
// in reverse topological order (leaves first).
func (s *Supervisor) stopDependents(name string) {
	deps := s.g.Dependents(name)
	// Reverse: stop leaves before their parents.
	for i := len(deps) - 1; i >= 0; i-- {
		dep := deps[i]
		if !dep.Watch {
			s.stopService(dep.Name, false)
		}
	}
}

func (s *Supervisor) reloadService(name string) error {
	s.mu.RLock()
	st := s.states[name]
	pgid := st.pgid
	sig := st.cfg.ReloadSignal
	s.mu.RUnlock()

	if sig == "" {
		return fmt.Errorf("service %q has no reload_signal configured", name)
	}
	if pgid == 0 {
		return fmt.Errorf("service %q is not running", name)
	}
	sigNum, err := parseSignal(sig)
	if err != nil {
		return err
	}
	return syscall.Kill(-pgid, sigNum)
}

func (s *Supervisor) clearFailed(name string) {
	s.mu.Lock()
	st := s.states[name]
	if st.state == StateFailed {
		st.state = StateStopped
		st.recentCrashes = st.recentCrashes[:0]
	}
	s.mu.Unlock()
}

func (s *Supervisor) shutdownAll() {
	log.Printf("supervisor: shutting down all services")
	order := s.g.Order()

	// Snapshot pgids before sending any signals. We cannot rely on
	// s.states[name].pgid being cleared during shutdown because onProcessExited
	// runs in the event loop, which is blocked here. Instead we use
	// kill(-pgid, 0) below to poll actual process-group liveness.
	s.mu.RLock()
	pgids := make([]int, 0, len(order))
	for _, svc := range order {
		if !svc.Watch {
			if pgid := s.states[svc.Name].pgid; pgid != 0 {
				pgids = append(pgids, pgid)
			}
		}
	}
	s.mu.RUnlock()

	// Send SIGTERM in reverse topological order (leaves first) so that
	// dependents are asked to stop before their dependencies.
	for i := len(order) - 1; i >= 0; i-- {
		svc := order[i]
		if svc.Watch {
			continue
		}
		s.mu.RLock()
		pgid := s.states[svc.Name].pgid
		s.mu.RUnlock()
		if pgid != 0 {
			log.Printf("supervisor: stopping %s", svc.Name)
			s.killPgid(pgid, false) // SIGTERM
		}
	}

	if len(pgids) == 0 {
		return
	}

	// Poll until every process group has exited or the deadline passes.
	// kill(-pgid, 0) returns ESRCH once the group no longer exists.
	const maxWait = 10 * time.Second
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		n := 0
		for _, pgid := range pgids {
			if syscall.Kill(-pgid, 0) == nil {
				pgids[n] = pgid
				n++
			}
		}
		pgids = pgids[:n]
		if len(pgids) == 0 {
			return
		}
	}

	// Force-kill anything that survived the graceful window.
	for _, pgid := range pgids {
		if syscall.Kill(-pgid, 0) == nil {
			log.Printf("supervisor: force-killing pgid %d", pgid)
			syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck
		}
	}
	// Short pause for SIGKILL delivery before the process exits.
	time.Sleep(100 * time.Millisecond)
}

func (s *Supervisor) killPgid(pgid int, force bool) {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-pgid, sig); err != nil && err != syscall.ESRCH {
		log.Printf("supervisor: kill -%d %d: %v", sig, pgid, err)
	}
}

func parseSignal(name string) (syscall.Signal, error) {
	name = strings.ToUpper(strings.TrimPrefix(name, "SIG"))
	signals := map[string]syscall.Signal{
		"HUP":  syscall.SIGHUP,
		"INT":  syscall.SIGINT,
		"QUIT": syscall.SIGQUIT,
		"USR1": syscall.SIGUSR1,
		"USR2": syscall.SIGUSR2,
		"TERM": syscall.SIGTERM,
	}
	if sig, ok := signals[name]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", name)
}

// LogPath returns the expected disk log path for a service.
func (s *Supervisor) LogPath(name string) string {
	return filepath.Join(s.logDir, name+".log")
}

// Namespace returns the namespace directory 9init is watching.
func (s *Supervisor) Namespace() string {
	return s.ns
}

// ServiceNames returns all service names in topological order.
func (s *Supervisor) ServiceNames() []string {
	order := s.g.Order()
	names := make([]string, len(order))
	for i, svc := range order {
		names[i] = svc.Name
	}
	return names
}


