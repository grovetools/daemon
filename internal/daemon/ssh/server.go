// Package ssh provides an embedded SSH server for the grove daemon.
// It uses charmbracelet/wish to serve SSH connections with public key auth.
package ssh

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/creack/pty"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/util/pathutil"
	daemonpty "github.com/grovetools/daemon/internal/daemon/pty"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
)

// Server wraps a wish SSH server with grove-specific configuration.
type Server struct {
	srv        *ssh.Server
	ulog       *logging.UnifiedLogger
	port       int
	store      *store.Store
	ptyManager *daemonpty.Manager
}

// SetStore injects the daemon state store for workspace listing.
func (s *Server) SetStore(st *store.Store) {
	s.store = st
}

// SetPtyManager injects the PTY session manager for listing daemon-owned PTYs.
func (s *Server) SetPtyManager(pm *daemonpty.Manager) {
	s.ptyManager = pm
}

// New creates a new SSH server from daemon config.
// Returns nil if SSH is not enabled.
// The logger parameter is retained for backwards compatibility and will be
// removed in a later phase.
func New(cfg *config.DaemonSSHConfig, _ *logrus.Entry) (*Server, error) {
	if cfg == nil || cfg.Enabled == nil || !*cfg.Enabled {
		return nil, nil
	}

	port := 2222
	if cfg.Port > 0 {
		port = cfg.Port
	}

	hostKeyPath := paths.SSHHostKeyPath()
	if cfg.HostKeyPath != "" {
		expanded, err := pathutil.Expand(cfg.HostKeyPath)
		if err != nil {
			return nil, fmt.Errorf("expanding host key path: %w", err)
		}
		hostKeyPath = expanded
	}

	// Ensure host key directory exists
	if err := os.MkdirAll(filepath.Dir(hostKeyPath), 0700); err != nil {
		return nil, fmt.Errorf("creating host key directory: %w", err)
	}

	// Resolve authorized_keys path
	authorizedKeysPath, err := pathutil.Expand("~/.ssh/authorized_keys")
	if err != nil {
		return nil, fmt.Errorf("expanding authorized_keys path: %w", err)
	}

	addr := fmt.Sprintf(":%d", port)

	ulog := logging.NewUnifiedLogger("groved.ssh")
	s := &Server{
		ulog: ulog,
		port: port,
	}

	opts := []ssh.Option{
		wish.WithAddress(addr),
		wish.WithHostKeyPath(hostKeyPath),
		wish.WithMiddleware(groveHandler(s)),
	}

	// Only add authorized_keys auth if the file exists
	if _, err := os.Stat(authorizedKeysPath); err == nil {
		opts = append(opts, wish.WithAuthorizedKeys(authorizedKeysPath))
	} else {
		ulog.Warn("~/.ssh/authorized_keys not found, SSH server will accept all connections").Log(context.Background())
	}

	srv, err := wish.NewServer(opts...)
	if err != nil {
		return nil, fmt.Errorf("creating SSH server: %w", err)
	}

	s.srv = srv
	return s, nil
}

// Start begins listening for SSH connections. This blocks until the server
// is shut down or encounters an error.
func (s *Server) Start() error {
	s.ulog.Info("SSH server listening").Field("port", s.port).Log(context.Background())
	return s.srv.ListenAndServe()
}

// Stop gracefully shuts down the SSH server.
func (s *Server) Stop() error {
	s.ulog.Info("SSH server stopping").Log(context.Background())
	return s.srv.Close()
}

// resolveGrovetermPath finds the groveterm binary, checking BinDir first
// then falling back to PATH.
func resolveGrovetermPath() (string, error) {
	binDirPath := filepath.Join(paths.BinDir(), "groveterm")
	if _, err := os.Stat(binDirPath); err == nil {
		return binDirPath, nil
	}
	p, err := exec.LookPath("groveterm")
	if err != nil {
		return "", fmt.Errorf("groveterm not found in %s or PATH", paths.BinDir())
	}
	return p, nil
}

// groveHandler returns a wish middleware that routes SSH sessions based on
// command arguments (Phase 4) or shows a session picker (Phase 3).
func groveHandler(s *Server) wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(sess ssh.Session) {
			ctx := sess.Context()
			s.ulog.Info("SSH session connected").Field("user", sess.User()).Log(ctx)

			ptyReq, winCh, isPty := sess.Pty()
			if !isPty {
				wish.Println(sess, "PTY allocation is required. Use: ssh -t ...")
				return
			}

			binPath, err := resolveGrovetermPath()
			if err != nil {
				s.ulog.Error("failed to resolve groveterm binary").Err(err).Log(ctx)
				wish.Println(sess, "Error: "+err.Error())
				return
			}

			// Phase 4: Check for direct command arguments
			var args []string
			if cmdArgs := sess.Command(); len(cmdArgs) > 0 {
				args, err = s.resolveCommandArgs(cmdArgs)
				if err != nil {
					wish.Println(sess, "Error: "+err.Error())
					return
				}
			} else {
				// Phase 3: No args — show the session picker
				args, err = s.runPicker(sess, ptyReq)
				if err != nil {
					// User cancelled or picker error
					return
				}
			}

			cmd := exec.CommandContext(sess.Context(), binPath, args...)

			// Pass through environment, with TERM from the SSH client
			cmd.Env = os.Environ()
			cmd.Env = append(cmd.Env, fmt.Sprintf("TERM=%s", ptyReq.Term))
			for _, env := range sess.Environ() {
				if !strings.HasPrefix(env, "TERM=") {
					cmd.Env = append(cmd.Env, env)
				}
			}

			// Allocate a PTY for the child process
			ptmx, err := pty.Start(cmd)
			if err != nil {
				s.ulog.Error("failed to start groveterm with PTY").Err(err).Log(ctx)
				wish.Println(sess, "Error starting groveterm: "+err.Error())
				return
			}
			defer ptmx.Close()

			_ = pty.Setsize(ptmx, &pty.Winsize{
				Rows: uint16(ptyReq.Window.Height),
				Cols: uint16(ptyReq.Window.Width),
			})

			go func() {
				for win := range winCh {
					_ = pty.Setsize(ptmx, &pty.Winsize{
						Rows: uint16(win.Height),
						Cols: uint16(win.Width),
					})
				}
			}()

			go func() {
				_, _ = io.Copy(ptmx, sess)
			}()
			_, _ = io.Copy(sess, ptmx)

			if err := cmd.Wait(); err != nil {
				s.ulog.Debug("groveterm exited").Err(err).Log(ctx)
			}
		}
	}
}

// resolveCommandArgs parses SSH command arguments into groveterm args.
// Supports: follow <workspace>, attach <pty-id>, shell
func (s *Server) resolveCommandArgs(cmdArgs []string) ([]string, error) {
	switch cmdArgs[0] {
	case "follow":
		if len(cmdArgs) < 2 {
			return nil, fmt.Errorf("usage: follow <workspace>")
		}
		target := cmdArgs[1]
		if s.store != nil {
			if !s.workspaceExists(target) {
				return nil, fmt.Errorf("workspace %q not found", target)
			}
		}
		return []string{"--follower", "--workspace", target}, nil

	case "attach":
		if len(cmdArgs) < 2 {
			return nil, fmt.Errorf("usage: attach <pty-session-id>")
		}
		target := cmdArgs[1]
		if s.ptyManager != nil {
			if _, ok := s.ptyManager.Get(target); !ok {
				return nil, fmt.Errorf("PTY session %q not found", target)
			}
		}
		return []string{"--attach", target}, nil

	case "shell":
		return []string{"--shell"}, nil

	default:
		// Treat bare args as implicit "follow <workspace>"
		target := cmdArgs[0]
		if s.store != nil {
			if !s.workspaceExists(target) {
				return nil, fmt.Errorf("unknown command or workspace %q. Usage: follow|attach|shell", target)
			}
		}
		return []string{"--follower", "--workspace", target}, nil
	}
}

// workspaceExists checks if a workspace name exists in the store.
func (s *Server) workspaceExists(name string) bool {
	for _, ws := range s.store.GetWorkspaces() {
		if ws.Name == name {
			return true
		}
	}
	return false
}
