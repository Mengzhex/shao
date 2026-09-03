package mcpsrv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"tmon/internal/config"
	"tmon/internal/store"
)

// A running endpoint records where it is and how to stop it.
//
// This exists because the alternative is worse than untidy. Without a precise
// way to stop the server, the obvious move is to kill it by image name --
// `taskkill /F /IM tmon.exe`, `pkill tmon` -- and every recorder wrapping a
// live terminal has that same image name. Doing so kills the shells those
// recorders own, taking out terminals that had nothing to do with the server.
// The fix is to make stopping it precise, so nobody reaches for the blunt
// instrument.

const (
	serveStateFile = "serve.json"
	serveStopFile  = "serve.stop"
)

// ServeState describes a running HTTP endpoint.
//
// It carries enough to describe the endpoint fully from another process, so a
// detached server can be started by one command and reported by another
// without the second having to guess at any of it.
type ServeState struct {
	PID       int       `json:"pid"`
	Addr      string    `json:"addr"`
	URL       string    `json:"url"`
	StartedAt time.Time `json:"started_at"`
	// LANURLs is empty for a loopback-only endpoint.
	LANURLs []string `json:"lan_urls,omitempty"`
	Allow   string   `json:"allow,omitempty"`
}

func serveStatePath(cfg *config.Config) string {
	return filepath.Join(cfg.Root(), serveStateFile)
}

func serveStopPath(cfg *config.Config) string {
	return filepath.Join(cfg.Root(), serveStopFile)
}

// WriteServeState records a newly started endpoint.
func WriteServeState(cfg *config.Config, h *HTTPServer, allow string) error {
	// Clear any stale stop request, or a freshly started server would see a
	// leftover file and shut itself down immediately.
	_ = os.Remove(serveStopPath(cfg))

	data, err := json.MarshalIndent(ServeState{
		PID:       os.Getpid(),
		Addr:      h.Addr,
		URL:       h.URL(),
		StartedAt: time.Now(),
		LANURLs:   h.LANURLs(),
		Allow:     allow,
	}, "", "  ")
	if err != nil {
		return err
	}
	return config.WritePrivateFile(serveStatePath(cfg), append(data, '\n'))
}

// ClearServeState removes the record of a stopped endpoint.
func ClearServeState(cfg *config.Config) {
	_ = os.Remove(serveStatePath(cfg))
	_ = os.Remove(serveStopPath(cfg))
}

// ReadServeState returns the recorded endpoint, and whether its process is
// actually still running. A state file left behind by a killed server reports
// running=false rather than being trusted.
func ReadServeState(cfg *config.Config) (state ServeState, running bool) {
	data, err := os.ReadFile(serveStatePath(cfg))
	if err != nil {
		return ServeState{}, false
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return ServeState{}, false
	}
	return state, store.ProcessAlive(state.PID)
}

// RequestServeStop asks a running endpoint to shut down.
//
// A file rather than a signal, for the same reason recorders use one: it
// behaves identically on Windows and POSIX, and it lets the server finish
// cleanly instead of being killed.
func RequestServeStop(cfg *config.Config) error {
	return config.WritePrivateFile(serveStopPath(cfg), []byte("stop\n"))
}

// ServeStopRequested reports whether a shutdown has been asked for.
func ServeStopRequested(cfg *config.Config) bool {
	_, err := os.Stat(serveStopPath(cfg))
	return err == nil
}
