package apiserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolvePluginDirs locks in the resolver's precedence: explicit env
// override wins, then EVO_HOME canonical layout (if it exists), then the
// upward umbrella walk, then a last-resort relative fallback. The Pi5
// docker image relies on the EVO_HOME branch — regressing it would silently
// load zero plugins, exactly what this helper exists to prevent.
func TestResolvePluginDirs(t *testing.T) {
	// Snapshot/restore the two env vars we touch so the test doesn't
	// leak state into sibling tests in this package.
	t.Setenv("EVO_PLUGIN_DIRS", "")
	t.Setenv("EVO_HOME", "")

	t.Run("EVO_PLUGIN_DIRS wins", func(t *testing.T) {
		t.Setenv("EVO_PLUGIN_DIRS", "/a:/b:  :/c")
		got := resolvePluginDirs()
		require.Equal(t, []string{"/a", "/b", "/c"}, got, "trims whitespace and drops empty entries")
	})

	t.Run("EVO_HOME canonical layout when present", func(t *testing.T) {
		// Build a fake EVO_HOME with the expected plugins/builtin subtree.
		home := t.TempDir()
		for _, sub := range []string{"builtin", "community", "n8n"} {
			require.NoError(t, os.MkdirAll(filepath.Join(home, "plugins", sub), 0o755))
		}
		t.Setenv("EVO_PLUGIN_DIRS", "")
		t.Setenv("EVO_HOME", home)

		got := resolvePluginDirs()
		require.Equal(t, []string{
			filepath.Join(home, "plugins", "builtin"),
			filepath.Join(home, "plugins", "community"),
			filepath.Join(home, "plugins", "n8n"),
		}, got)
	})

	t.Run("upward umbrella walk finds workflow-builder plugins", func(t *testing.T) {
		// Simulate an umbrella checkout: $tmp/evolve/workflow-builder/plugins/{builtin,...}.
		// Resolver starts from a nested subdirectory and walks up to find it.
		tmp := t.TempDir()
		pluginsRoot := filepath.Join(tmp, "evolve", "workflow-builder", "plugins")
		for _, sub := range []string{"builtin", "community", "n8n"} {
			require.NoError(t, os.MkdirAll(filepath.Join(pluginsRoot, sub), 0o755))
		}
		nested := filepath.Join(tmp, "platform", "dashboard", "apiserver")
		require.NoError(t, os.MkdirAll(nested, 0o755))

		// Point EVO_HOME at a directory with no plugins/ so the canonical
		// branch falls through to the upward walk.
		emptyHome := t.TempDir()
		t.Setenv("EVO_PLUGIN_DIRS", "")
		t.Setenv("EVO_HOME", emptyHome)

		origWd, err := os.Getwd()
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origWd) })
		require.NoError(t, os.Chdir(nested))

		got := resolvePluginDirs()
		require.Equal(t, []string{
			filepath.Join(pluginsRoot, "builtin"),
			filepath.Join(pluginsRoot, "community"),
			filepath.Join(pluginsRoot, "n8n"),
		}, got)
	})
}
