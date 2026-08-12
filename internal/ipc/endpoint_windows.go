//go:build windows

package ipc

import (
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/Microsoft/go-winio"
)

// stateDir roots the lock files (the endpoint itself is a named pipe, not a
// filesystem path): %LOCALAPPDATA%\agenthooks via os.UserCacheDir, falling
// back to the system temp dir.
func stateDir() (string, error) {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "agenthooks"), nil
	}
	return filepath.Join(os.TempDir(), "agenthooks"), nil
}

func endpoint(_, id string) string {
	return `\\.\pipe\agenthooks-` + id
}

func lockPath(dir, id, suffix string) string {
	return filepath.Join(dir, "agenthooks-"+id+suffix)
}

// validateEndpoint vets an explicit --socket value. Named-pipe names have
// no sun_path-style length constraint, so everything passes.
func validateEndpoint(string) error {
	return nil
}

// Listen creates the named pipe. Pipe instances vanish with their process,
// so there is no stale-endpoint sweep here; a creation failure with a live
// listener behind it maps to ErrAlreadyRunning.
func Listen(endpoint string) (net.Listener, error) {
	ln, err := winio.ListenPipe(endpoint, nil)
	if err == nil {
		return ln, nil
	}
	if dialProbe(endpoint) {
		return nil, ErrAlreadyRunning
	}
	return nil, err
}

// Dial connects to a listening server, bounded by timeout.
func Dial(endpoint string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(endpoint, &timeout)
}
