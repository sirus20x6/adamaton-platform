// Package manifest loads + validates the per-plugin JSON descriptors the
// host scans at startup. A manifest declares identity, the subprocess
// command, transport, capabilities, and supervisor knobs. The host
// refuses to load a plugin whose manifest fails validation.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Manifest mirrors the on-disk JSON. Field order doesn't matter; tags
// pin the JSON spelling. ConfigSchema / ArgsSchema are loose maps because
// plugin-side schemas vary in shape and the host doesn't interpret them
// beyond passing them to the frontend.
type Manifest struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Version      string         `json:"version"`
	Category     string         `json:"category"`
	Icon         string         `json:"icon"`
	Capabilities []string       `json:"capabilities"`
	Command      []string       `json:"command"`
	Transport    string         `json:"transport"`
	ConfigSchema map[string]any `json:"config_schema"`
	ArgsSchema   map[string]any `json:"args_schema"`
	Supervisor   SupervisorOpts `json:"supervisor"`
}

// SupervisorOpts is the subset of lifecycle knobs the manifest controls.
// Anything more dynamic (e.g. per-call timeouts) lives in args_schema.
type SupervisorOpts struct {
	IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
	MaxRestartPerMin   int `json:"max_restart_per_min"`
}

// idPattern enforces a kebab-ish slug. The 64-char cap leaves headroom
// for socket filenames inside /run/dr-plugins without bumping into the
// 108-byte sockaddr_un limit (path + pid suffix).
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Load reads one manifest file. Returned errors wrap the file path so
// LoadAll can surface a per-file map without callers re-annotating.
func Load(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &m, nil
}

// LoadAll scans dir for *.json and returns the valid set plus a per-file
// error map so the caller can log the bad ones without aborting startup.
// A missing dir is not an error (returns empty maps) so dev environments
// without /etc/deepresearch/plugins keep booting.
func LoadAll(dir string) (map[string]*Manifest, map[string]error) {
	out := map[string]*Manifest{}
	errs := map[string]error{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, errs
		}
		errs[dir] = err
		return out, errs
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		m, err := Load(p)
		if err != nil {
			errs[p] = err
			continue
		}
		if _, dup := out[m.ID]; dup {
			errs[p] = fmt.Errorf("duplicate plugin id %q", m.ID)
			continue
		}
		out[m.ID] = m
	}
	return out, errs
}

func (m *Manifest) validate() error {
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("id %q must match %s", m.ID, idPattern)
	}
	if strings.TrimSpace(m.Category) == "" {
		return errors.New("category is required")
	}
	if len(m.Capabilities) == 0 {
		return errors.New("capabilities is required")
	}
	if len(m.Command) == 0 {
		return errors.New("command is required")
	}
	if m.Transport != "grpc-unix" {
		return fmt.Errorf("transport %q unsupported (only grpc-unix today)", m.Transport)
	}
	// Capabilities are namespaced by category. Refuse extras so a misfiled
	// manifest can't claim verbs the host won't dispatch anyway.
	prefix := m.Category + "."
	for _, c := range m.Capabilities {
		if !strings.HasPrefix(c, prefix) {
			return fmt.Errorf("capability %q must start with %q", c, prefix)
		}
	}
	return nil
}
