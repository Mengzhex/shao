// Package config loads and persists shao's on-disk configuration.
//
// Everything shao needs lives under a single root directory (default
// ~/.shao), so the tool stays self-contained and easy to wipe:
//
//	~/.shao/config.yaml     this file's contents
//	~/.shao/sessions/<id>/  per-session ring buffers (see internal/store)
//	~/.shao/keys/           dedicated read-only SSH keys, one per host
//	~/.shao/token           bearer token for the local HTTP MCP endpoint
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Buffer sizing defaults. The rationale for MaxBytes is documented for the
// user in docs/buffer-sizing.md: heavy log spam runs 2-5 MB/min per session,
// so 256 MiB covers roughly 1-2 hours of continuous scrollback.
const (
	DefaultSegmentBytes   int64 = 4 << 20   // 4 MiB
	DefaultMaxBytes       int64 = 256 << 20 // 256 MiB of raw bytes per session
	DefaultCookedMaxBytes int64 = 64 << 20  // 64 MiB of readable line stream
	DefaultGlobalDiskCap  int64 = 4 << 30   // 4 GiB across all sessions
	// DefaultRetainDays bounds history by age as well as by size. A size cap
	// alone means sessions accumulate indefinitely on a quiet machine and are
	// only reclaimed when the total happens to hit the limit.
	DefaultRetainDays = 7

	// Flush discipline. Recorders are separate processes from the MCP
	// readers, so freshness is bounded by these two numbers rather than by
	// shared memory: under continuous output a reader is at most
	// FlushIntervalMS behind, and the moment output goes quiet (which is
	// when a human notices an error and asks about it) IdleFlushMS applies.
	DefaultFlushIntervalMS = 200
	DefaultIdleFlushMS     = 50

	// DefaultHTTPPort is fixed rather than left to the OS. A URL is pasted
	// into an AI client's configuration once and expected to keep working; a
	// port chosen at random would change on every restart and silently break
	// that configuration.
	DefaultHTTPPort = 7337
)

type BufferConfig struct {
	MaxBytes        int64 `yaml:"max_bytes"`
	CookedMaxBytes  int64 `yaml:"cooked_max_bytes"`
	SegmentBytes    int64 `yaml:"segment_bytes"`
	GlobalDiskCap   int64 `yaml:"global_disk_cap"`
	FlushIntervalMS int   `yaml:"flush_interval_ms"`
	IdleFlushMS     int   `yaml:"idle_flush_ms"`
	// RetainDays deletes finished sessions older than this. 0 uses the
	// default; a negative value keeps everything.
	RetainDays int `yaml:"retain_days"`
}

type RedactConfig struct {
	// Builtin enables the shipped credential patterns (see internal/redact).
	Builtin bool `yaml:"builtin"`
	// Patterns are extra user-supplied Go regexps, applied on top.
	Patterns []string `yaml:"patterns"`
	// ReadTimeSecondPass re-runs redaction on the way out to the AI, so a
	// gap in the capture-side rules is still caught before data leaves.
	ReadTimeSecondPass bool `yaml:"read_time_second_pass"`
}

type HTTPConfig struct {
	Enabled bool `yaml:"enabled"`
	// Bind defaults to 127.0.0.1. Setting it to an address other than
	// loopback exposes recorded terminal history to the network, so it is
	// only honoured when the caller passes AllowRemote as well.
	Bind string `yaml:"bind"`
	// Port defaults to DefaultHTTPPort. 0 asks the OS for any free port,
	// which is useful in tests but means the URL changes on every restart.
	Port int `yaml:"port"`
	// AllowRemote must be set deliberately, by a command-line flag, before a
	// non-loopback Bind takes effect. It exists so that a stray value in a
	// config file cannot quietly publish a terminal to the network.
	AllowRemote bool `yaml:"-"`
	// Allow restricts which clients may connect, as a list of CIDRs such as
	// 192.168.1.0/24. Empty means any address that can reach the port, which
	// on a non-loopback bind is a much larger set than it sounds.
	Allow []string `yaml:"allow"`
}

type MCPConfig struct {
	Stdio bool       `yaml:"stdio"`
	HTTP  HTTPConfig `yaml:"http"`
}

// SessionRule pins per-label settings, so a known-noisy session can get a
// bigger ring than the global default and can be attributed to a host.
type SessionRule struct {
	Label    string `yaml:"label"`
	Host     string `yaml:"host"`
	MaxBytes int64  `yaml:"max_bytes"`
}

// Enforcement records how read-only access to a host is guaranteed. shao
// deliberately offers no soft, in-process-only tier: a host that cannot be
// configured for sshd-enforced read-only access is simply not queryable.
type Enforcement string

const (
	// EnforceForcedCommandRoot is the strongest tier: a dedicated account
	// plus both an authorized_keys forced command and an sshd_config
	// Match block, so one misconfiguration still leaves a second layer.
	EnforceForcedCommandRoot Enforcement = "forced_command_root"
	// EnforceForcedCommandUser needs no root: a dedicated key in your own
	// ~/.ssh/authorized_keys carrying command="..." plus restrict. sshd
	// still does the enforcing, so it is a system-layer guarantee.
	EnforceForcedCommandUser Enforcement = "forced_command_user"
	// EnforceDisabled means environment queries are refused for this host.
	EnforceDisabled Enforcement = "disabled"
)

// Queryable reports whether this tier permits environment queries at all.
func (e Enforcement) Queryable() bool {
	return e == EnforceForcedCommandRoot || e == EnforceForcedCommandUser
}

func (e Enforcement) valid() bool {
	return e.Queryable() || e == EnforceDisabled
}

type HostConfig struct {
	Name        string      `yaml:"name"`
	Address     string      `yaml:"address"`
	Port        int         `yaml:"port"`
	User        string      `yaml:"user"`
	Enforcement Enforcement `yaml:"enforcement"`
	// IdentityFile is a key dedicated to the read-only probe, kept separate
	// from your day-to-day SSH key so it can be revoked on its own.
	IdentityFile string `yaml:"identity_file"`
	// HostKey pins the server key fingerprint. Empty means unpinned, which
	// Validate reports as a warning.
	HostKey   string   `yaml:"host_key"`
	ProbePath string   `yaml:"probe_path"`
	Aspects   []string `yaml:"aspects"`
	// VerifiedAt is set by `shao host verify` once enforcement has been
	// actively proven rather than merely declared.
	VerifiedAt string `yaml:"verified_at"`
}

func (h HostConfig) SSHPort() int {
	if h.Port == 0 {
		return 22
	}
	return h.Port
}

type Config struct {
	Buffer   BufferConfig  `yaml:"buffer"`
	Redact   RedactConfig  `yaml:"redact"`
	MCP      MCPConfig     `yaml:"mcp"`
	Sessions []SessionRule `yaml:"sessions"`
	Hosts    []HostConfig  `yaml:"hosts"`

	// root is where this config was loaded from; not serialized.
	root string
}

func Default() *Config {
	return &Config{
		Buffer: BufferConfig{
			MaxBytes:        DefaultMaxBytes,
			CookedMaxBytes:  DefaultCookedMaxBytes,
			SegmentBytes:    DefaultSegmentBytes,
			GlobalDiskCap:   DefaultGlobalDiskCap,
			FlushIntervalMS: DefaultFlushIntervalMS,
			IdleFlushMS:     DefaultIdleFlushMS,
			RetainDays:      DefaultRetainDays,
		},
		Redact: RedactConfig{Builtin: true, ReadTimeSecondPass: true},
		MCP: MCPConfig{
			Stdio: true,
			HTTP:  HTTPConfig{Enabled: true, Bind: "127.0.0.1", Port: DefaultHTTPPort},
		},
		root: DefaultRoot(),
	}
}

// DefaultRoot is ~/.shao, overridable with SHAO_HOME for tests and for
// users who keep tooling state elsewhere.
func DefaultRoot() string {
	if v := os.Getenv("SHAO_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".shao"
	}
	return filepath.Join(home, ".shao")
}

func (c *Config) Root() string        { return c.root }
func (c *Config) SessionsDir() string { return filepath.Join(c.root, "sessions") }
func (c *Config) KeysDir() string     { return filepath.Join(c.root, "keys") }
func (c *Config) TokenPath() string   { return filepath.Join(c.root, "token") }
func (c *Config) ConfigPath() string  { return filepath.Join(c.root, "config.yaml") }

// ProbeBinaryPath is where `shao host add` stages the probe executable
// before uploading it to a target host.
func (c *Config) ProbeBinaryPath() string {
	return filepath.Join(c.root, "probe", "shao-probe")
}

// Load reads root/config.yaml, filling in defaults for anything absent. A
// missing file is not an error: first run gets defaults, and a config file
// is written on demand by Save.
func Load(root string) (*Config, error) {
	if root == "" {
		root = DefaultRoot()
	}
	cfg := Default()
	cfg.root = root

	data, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	// Decode onto the defaults so keys absent from the file keep them.
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Join(root, "config.yaml"), err)
	}
	cfg.root = root
	cfg.applyFallbacks()
	return cfg, nil
}

// applyFallbacks repairs zero values that a partial config file can leave
// behind: yaml.Unmarshal writes 0 over a default when a key is present but
// empty.
func (c *Config) applyFallbacks() {
	b := &c.Buffer
	if b.SegmentBytes <= 0 {
		b.SegmentBytes = DefaultSegmentBytes
	}
	if b.MaxBytes <= 0 {
		b.MaxBytes = DefaultMaxBytes
	}
	if b.CookedMaxBytes <= 0 {
		b.CookedMaxBytes = DefaultCookedMaxBytes
	}
	if b.GlobalDiskCap <= 0 {
		b.GlobalDiskCap = DefaultGlobalDiskCap
	}
	if b.FlushIntervalMS <= 0 {
		b.FlushIntervalMS = DefaultFlushIntervalMS
	}
	if b.IdleFlushMS <= 0 {
		b.IdleFlushMS = DefaultIdleFlushMS
	}
	if b.RetainDays == 0 {
		b.RetainDays = DefaultRetainDays
	}
	// A ring must hold at least two segments, otherwise eviction would
	// throw away the segment currently being written.
	if b.MaxBytes < 2*b.SegmentBytes {
		b.MaxBytes = 2 * b.SegmentBytes
	}
	if b.CookedMaxBytes < 2*b.SegmentBytes {
		b.CookedMaxBytes = 2 * b.SegmentBytes
	}
	if c.MCP.HTTP.Bind == "" {
		c.MCP.HTTP.Bind = "127.0.0.1"
	}
}

// Validate returns fatal problems as an error and softer concerns as
// warnings for the caller to print.
func (c *Config) Validate() (warnings []string, err error) {
	seen := map[string]bool{}
	for i := range c.Hosts {
		h := &c.Hosts[i]
		if h.Name == "" {
			return warnings, fmt.Errorf("hosts[%d]: name is required", i)
		}
		if seen[h.Name] {
			return warnings, fmt.Errorf("hosts[%d]: duplicate host name %q", i, h.Name)
		}
		seen[h.Name] = true
		if h.Address == "" {
			return warnings, fmt.Errorf("host %q: address is required", h.Name)
		}
		if h.Enforcement == "" {
			// Refusing to guess is the point: an unset tier must never
			// silently become queryable.
			h.Enforcement = EnforceDisabled
			warnings = append(warnings, fmt.Sprintf(
				"host %q: enforcement not set, treating as disabled (run `shao host add %s`)", h.Name, h.Name))
			continue
		}
		if !h.Enforcement.valid() {
			return warnings, fmt.Errorf("host %q: unknown enforcement %q (want %s, %s or %s)",
				h.Name, h.Enforcement, EnforceForcedCommandRoot, EnforceForcedCommandUser, EnforceDisabled)
		}
		if !h.Enforcement.Queryable() {
			continue
		}
		if h.IdentityFile == "" {
			return warnings, fmt.Errorf("host %q: identity_file is required for enforcement %s", h.Name, h.Enforcement)
		}
		if h.VerifiedAt == "" {
			warnings = append(warnings, fmt.Sprintf(
				"host %q: enforcement never verified, environment queries stay disabled until `shao host verify %s` passes",
				h.Name, h.Name))
		}
		if h.HostKey == "" {
			warnings = append(warnings, fmt.Sprintf(
				"host %q: host_key not pinned; a machine-in-the-middle could impersonate this host", h.Name))
		}
	}
	return warnings, nil
}

// Host looks up a host by name.
func (c *Config) Host(name string) (*HostConfig, bool) {
	for i := range c.Hosts {
		if strings.EqualFold(c.Hosts[i].Name, name) {
			return &c.Hosts[i], true
		}
	}
	return nil, false
}

// RuleForLabel returns the session rule matching a label, if any.
func (c *Config) RuleForLabel(label string) (SessionRule, bool) {
	for _, r := range c.Sessions {
		if r.Label != "" && strings.EqualFold(r.Label, label) {
			return r, true
		}
	}
	return SessionRule{}, false
}

// MaxBytesForLabel resolves the ring size for a session, honouring a
// per-label override.
func (c *Config) MaxBytesForLabel(label string) int64 {
	if r, ok := c.RuleForLabel(label); ok && r.MaxBytes > 0 {
		return r.MaxBytes
	}
	return c.Buffer.MaxBytes
}

// HostForLabel resolves the host a labelled session belongs to, if pinned.
func (c *Config) HostForLabel(label string) string {
	if r, ok := c.RuleForLabel(label); ok {
		return r.Host
	}
	return ""
}

// Save writes the config back to disk with owner-only permissions. The
// buffers can contain credential-shaped text even after redaction, so the
// whole tree is kept private.
func (c *Config) Save() error {
	if err := EnsurePrivateDir(c.root); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# shao configuration. See docs/buffer-sizing.md for how to size the\n" +
		"# ring buffers, and docs/security-model.md for the enforcement tiers.\n"
	return WritePrivateFile(c.ConfigPath(), append([]byte(header), data...))
}

// ExpandUser resolves a leading ~ in a configured path.
func ExpandUser(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	rest := strings.TrimPrefix(p, "~")
	rest = strings.TrimLeft(rest, `/\`)
	return filepath.Join(home, rest)
}
