// Worker-types catalog: per-worker docker compose fragments loaded at
// startup from CATALOG_DIR (default /etc/adamaton/worker-types). Used
// by the /provision handler to materialise service blocks on hosts
// that don't already declare them in docker-compose.yml.
//
// Files live in the docker image at /etc/adamaton/worker-types/*.yml,
// copied there by the deploy-agent Dockerfile. Operators can override
// the catalog at runtime by bind-mounting their own directory onto
// /etc/adamaton/worker-types.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const defaultCatalogDir = "/etc/adamaton/worker-types"

// catalogEntry is the on-disk schema of a worker-types/*.yml file.
// Just the `service:` top-level key wrapping the full compose block.
// We hold the parsed yaml.Node so we can splice it directly into the
// override compose file without re-encoding round-trip drift.
type catalogEntry struct {
	Service yaml.Node `yaml:"service"`
}

var (
	catalogOnce sync.Once
	catalogVal  map[string]*catalogEntry
	catalogErr  error
)

// loadCatalog reads CATALOG_DIR once and caches the parsed entries.
// Missing directory is NOT an error -- the agent still serves /scale
// /restart on already-provisioned services; /provision returns 404
// for any service not in the empty catalog.
func loadCatalog() (map[string]*catalogEntry, error) {
	catalogOnce.Do(func() {
		dir := os.Getenv("CATALOG_DIR")
		if dir == "" {
			dir = defaultCatalogDir
		}
		out := map[string]*catalogEntry{}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// No catalog → empty map, no error.
				catalogVal = out
				return
			}
			catalogErr = fmt.Errorf("read %s: %w", dir, err)
			return
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				catalogErr = fmt.Errorf("read %s/%s: %w", dir, e.Name(), err)
				return
			}
			var ce catalogEntry
			if err := yaml.Unmarshal(b, &ce); err != nil {
				catalogErr = fmt.Errorf("parse %s/%s: %w", dir, e.Name(), err)
				return
			}
			if ce.Service.Kind == 0 {
				catalogErr = fmt.Errorf("%s/%s: missing top-level `service:` key", dir, e.Name())
				return
			}
			svc := strings.TrimSuffix(filepath.Base(e.Name()), ".yml")
			out[svc] = &ce
		}
		catalogVal = out
	})
	return catalogVal, catalogErr
}

// catalogNames returns the sorted list of services in the catalog.
// Empty slice if catalog dir is absent or empty.
func catalogNames() []string {
	c, err := loadCatalog()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// catalogHas reports whether svc is in the worker-types catalog.
func catalogHas(svc string) bool {
	c, err := loadCatalog()
	if err != nil {
		return false
	}
	_, ok := c[svc]
	return ok
}

// overrideUpsert reads the existing docker-compose.workers.yml at
// `path` (or creates an empty one if absent), inserts/replaces the
// `services.<svc>` block from the catalog, and writes back. Returns
// (added, error) where `added` is true if this is a fresh insert
// (false on idempotent re-run with the same block).
//
// The compose YAML schema we target:
//
//	services:
//	  skills-worker: { ...block... }
//	  reindex-worker: { ...block... }
//	volumes:
//	  nano_workspace: {}    # only declared if the block uses it
func overrideUpsert(path, svc string) (added bool, err error) {
	c, err := loadCatalog()
	if err != nil {
		return false, err
	}
	entry, ok := c[svc]
	if !ok {
		return false, fmt.Errorf("catalog: no such worker type %q", svc)
	}

	// Read existing override file (or seed empty).
	var root yaml.Node
	raw, readErr := readFileOptional(path)
	if readErr != nil {
		return false, readErr
	}
	if len(raw) == 0 {
		raw = []byte("services: {}\n")
	}
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 {
		return false, fmt.Errorf("%s: expected single document, got %d nodes", path, len(root.Content))
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return false, fmt.Errorf("%s: top-level must be a mapping", path)
	}

	servicesNode := mappingValue(top, "services")
	if servicesNode == nil {
		top.Content = append(top.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "services"},
			&yaml.Node{Kind: yaml.MappingNode},
		)
		servicesNode = top.Content[len(top.Content)-1]
	}
	if servicesNode.Kind != yaml.MappingNode {
		return false, fmt.Errorf("%s: services must be a mapping", path)
	}

	existingIdx := -1
	for i := 0; i < len(servicesNode.Content); i += 2 {
		if servicesNode.Content[i].Value == svc {
			existingIdx = i
			break
		}
	}
	svcBlock := entry.Service
	if existingIdx >= 0 {
		servicesNode.Content[existingIdx+1] = &svcBlock
		added = false
	} else {
		servicesNode.Content = append(servicesNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: svc},
			&svcBlock,
		)
		added = true
	}

	// Volume awareness: declare any named volumes from the block at
	// the top level so compose creates them on `up`.
	if vols := mappingValue(&svcBlock, "volumes"); vols != nil && vols.Kind == yaml.SequenceNode {
		for _, v := range vols.Content {
			if v.Kind != yaml.ScalarNode {
				continue
			}
			parts := strings.SplitN(v.Value, ":", 2)
			if len(parts) == 0 || strings.HasPrefix(parts[0], "/") || strings.HasPrefix(parts[0], ".") {
				continue
			}
			ensureNamedVolume(top, parts[0])
		}
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return added, fmt.Errorf("marshal %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return added, fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return added, fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return added, nil
}

func ensureNamedVolume(top *yaml.Node, name string) {
	volsNode := mappingValue(top, "volumes")
	if volsNode == nil {
		top.Content = append(top.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "volumes"},
			&yaml.Node{Kind: yaml.MappingNode},
		)
		volsNode = top.Content[len(top.Content)-1]
	}
	if volsNode.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(volsNode.Content); i += 2 {
		if volsNode.Content[i].Value == name {
			return
		}
	}
	volsNode.Content = append(volsNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: name},
		&yaml.Node{Kind: yaml.MappingNode},
	)
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func readFileOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}
