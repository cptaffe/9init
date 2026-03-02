// Package fs9p serves the 9P filesystem at $NAMESPACE/init.
//
// Layout:
//
//	init/
//	├── ctl        write-only; accepts global control commands
//	├── status     read-only;  one line per service
//	└── svc/
//	    └── <name>/
//	        ├── status    current state string
//	        ├── pid       PID or "-"
//	        ├── uptime    seconds since start or "-"
//	        ├── restarts  restart count
//	        └── log       streaming; reads block for new output
//
// The server is posted at $NAMESPACE/init via a 9pserve(1) socketpair,
// the same mechanism used by acme-styles and other plan9port services.
package fs9p

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"os"
	"strconv"
	"strings"
	"time"

	"9fans.net/go/plan9"
	"9fans.net/go/plan9/srv9p"
	"github.com/cptaffe/9init/internal/logwriter"
	"github.com/cptaffe/9init/internal/supervisor"
	"golang.org/x/sys/unix"
)

// fileRole identifies the purpose of a file node in the tree.
type fileRole int

const (
	roleCtl      fileRole = iota // init/ctl
	roleStatus                   // init/status
	roleSvcStatus                // init/svc/<name>/status
	roleSvcPid                   // init/svc/<name>/pid
	roleSvcUptime                // init/svc/<name>/uptime
	roleSvcRestarts              // init/svc/<name>/restarts
	roleSvcLog                   // init/svc/<name>/log (streaming)
)

// fileAux is stored in File.Aux for each non-directory node.
type fileAux struct {
	role    fileRole
	svcName string // non-empty for per-service files
}

// subAux is stored in Fid.Aux when a log file is opened.
type subAux struct {
	sub *logwriter.Subscription
}

// Server wraps a supervisor and serves the 9P tree.
type Server struct {
	sup  *supervisor.Supervisor
	srvP *srv9p.Server
}

// Listen creates the 9P server, posts it at srvPath using 9pserve, and
// returns a Server. Call Serve to start handling requests.
func Listen(srvPath string, sup *supervisor.Supervisor) (*Server, io.ReadWriteCloser, func(), error) {
	rw, cleanup, err := listen(srvPath)
	if err != nil {
		return nil, nil, nil, err
	}
	fs := &Server{sup: sup}
	fs.srvP = fs.buildServer()
	return fs, rw, cleanup, nil
}

// Serve blocks, handling 9P requests from rw. Returns when the connection
// is closed (typically when the 9pserve process exits).
func (fs *Server) Serve(rw io.ReadWriteCloser) {
	fs.srvP.Serve(rw, rw)
}

// buildServer constructs the srv9p.Server with the static file tree and
// dynamic read/write callbacks.
func (fs *Server) buildServer() *srv9p.Server {
	uid := currentUser()
	tree := srv9p.NewTree(uid, uid, plan9.DMDIR|0o555, nil)
	root := tree.Root

	// init/ctl
	ctlFile, _ := root.Create("ctl", uid, 0o222, &fileAux{role: roleCtl})
	_ = ctlFile

	// init/status
	statusFile, _ := root.Create("status", uid, 0o444, &fileAux{role: roleStatus})
	_ = statusFile

	// init/svc/
	svcDir, _ := root.Create("svc", uid, plan9.DMDIR|0o555, nil)

	// init/svc/<name>/ for each service
	for _, name := range fs.sup.ServiceNames() {
		n := name // capture
		dir, _ := svcDir.Create(n, uid, plan9.DMDIR|0o555, nil)
		dir.Create("status", uid, 0o444, &fileAux{role: roleSvcStatus, svcName: n})
		dir.Create("pid", uid, 0o444, &fileAux{role: roleSvcPid, svcName: n})
		dir.Create("uptime", uid, 0o444, &fileAux{role: roleSvcUptime, svcName: n})
		dir.Create("restarts", uid, 0o444, &fileAux{role: roleSvcRestarts, svcName: n})
		if fs.sup.LogWriter(n) != nil {
			dir.Create("log", uid, 0o444, &fileAux{role: roleSvcLog, svcName: n})
		}
	}

	srv := &srv9p.Server{
		Tree: tree,

		Open: func(ctx context.Context, fid *srv9p.Fid, mode uint8) error {
			aux, ok := fid.File().Aux.(*fileAux)
			if !ok || aux.role != roleSvcLog {
				return nil
			}
			lw := fs.sup.LogWriter(aux.svcName)
			if lw == nil {
				return fmt.Errorf("no log for %s", aux.svcName)
			}
			fid.SetAux(&subAux{sub: lw.Subscribe()})
			return nil
		},

		Read: func(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
			// Streaming log read: ignore offset; deliver from subscription.
			if sa, ok := fid.Aux().(*subAux); ok {
				return sa.sub.Read(ctx, data)
			}

			// Static file read: generate content and serve at offset.
			aux, ok := fid.File().Aux.(*fileAux)
			if !ok {
				return 0, nil
			}
			content := fs.readContent(aux)
			return fid.ReadBytes(data, offset, []byte(content))
		},

		Write: func(ctx context.Context, fid *srv9p.Fid, data []byte, offset int64) (int, error) {
			aux, ok := fid.File().Aux.(*fileAux)
			if !ok || aux.role != roleCtl {
				return 0, fmt.Errorf("not writable")
			}
			cmd := strings.TrimRight(string(data), "\n\r")
			if err := fs.sup.Control(cmd); err != nil {
				return 0, err
			}
			return len(data), nil
		},

		Clunk: func(fid *srv9p.Fid) {
			if sa, ok := fid.Aux().(*subAux); ok {
				sa.sub.Close()
				fid.SetAux(nil)
			}
		},
	}
	return srv
}

// readContent generates the string content of a static file.
func (fs *Server) readContent(aux *fileAux) string {
	switch aux.role {
	case roleStatus:
		return fs.formatStatus()
	case roleSvcStatus:
		snap, ok := fs.sup.Snapshot(aux.svcName)
		if !ok {
			return "unknown\n"
		}
		return snap.State.String() + "\n"
	case roleSvcPid:
		snap, ok := fs.sup.Snapshot(aux.svcName)
		if !ok || snap.Pid == 0 {
			return "-\n"
		}
		return strconv.Itoa(snap.Pid) + "\n"
	case roleSvcUptime:
		snap, ok := fs.sup.Snapshot(aux.svcName)
		if !ok || snap.Started.IsZero() {
			return "-\n"
		}
		return fmt.Sprintf("%.0f\n", time.Since(snap.Started).Seconds())
	case roleSvcRestarts:
		snap, ok := fs.sup.Snapshot(aux.svcName)
		if !ok {
			return "0\n"
		}
		return strconv.Itoa(snap.Restarts) + "\n"
	}
	return "\n"
}

func (fs *Server) formatStatus() string {
	snaps := fs.sup.AllSnapshots()
	var sb strings.Builder
	for _, s := range snaps {
		pid := "-"
		if s.Pid != 0 {
			pid = strconv.Itoa(s.Pid)
		}
		uptime := "-"
		if !s.Started.IsZero() {
			uptime = fmt.Sprintf("%.0fs", time.Since(s.Started).Seconds())
		}
		fmt.Fprintf(&sb, "%-20s %-10s pid=%-8s uptime=%-10s restarts=%d\n",
			s.Name, s.State, pid, uptime, s.Restarts)
	}
	return sb.String()
}

// listen removes any stale socket at srvPath, launches 9pserve to listen
// on it, and returns the server end of a socketpair for 9P conversation.
// The cleanup function kills 9pserve and closes the connection.
func listen(srvPath string) (io.ReadWriteCloser, func(), error) {
	os.Remove(srvPath)

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	parent := os.NewFile(uintptr(fds[0]), "9init-srv")
	child := os.NewFile(uintptr(fds[1]), "9init-9pserve")

	cmd := exec.Command("9pserve", "unix!"+srvPath)
	cmd.Stdin = child
	cmd.Stdout = child
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		parent.Close()
		child.Close()
		return nil, nil, fmt.Errorf("9pserve: %w", err)
	}
	child.Close()

	cleanup := func() {
		parent.Close()
		cmd.Wait() //nolint:errcheck
	}
	return parent, cleanup, nil
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "none"
}
