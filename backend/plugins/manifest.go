// File overview: Runtime plugin manifest discovery for packaged plugin assets.

package plugins

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"rolltop/backend/logging"
)

var manifestIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Manifest describes one packaged plugin folder. Backend binaries and frontend
// modules are intentionally metadata-only here; the web layer serves declared
// assets and can grow process management against this stable shape.
type Manifest struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	Version          string          `json:"version"`
	EnabledByDefault bool            `json:"enabled_by_default"`
	Heavy            bool            `json:"heavy"`
	Experimental     bool            `json:"experimental"`
	AutoStart        bool            `json:"auto_start"`
	Backend          *BackendBundle  `json:"backend,omitempty"`
	Frontend         *FrontendBundle `json:"frontend,omitempty"`
	Themes           []ThemeBundle   `json:"themes,omitempty"`
	Migrations       []Migration     `json:"-"`
	Dir              string          `json:"-"`
}

// BackendBundle is the manifest hook for managed plugin binaries.
type BackendBundle struct {
	Kind   string `json:"kind"`
	Binary string `json:"binary"`
}

// FrontendBundle is the manifest hook for browser-side plugin modules.
type FrontendBundle struct {
	Module string `json:"module"`
	CSS    string `json:"css,omitempty"`
}

// ThemeBundle describes one CSS-plus-assets theme supplied by a plugin.
type ThemeBundle struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	CSS  string `json:"css"`
}

// LoadManifests reads every direct child manifest.json in root. A missing root
// is valid so source builds and tests do not require an installed plugin tree.
func LoadManifests(root string) ([]Manifest, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out []Manifest
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifestPath := filepath.Join(root, entry.Name(), "manifest.json")
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		var manifest Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return nil, fmt.Errorf("%s: %w", manifestPath, err)
		}
		manifest.Dir = filepath.Dir(manifestPath)
		if err := validateManifest(manifest); err != nil {
			return nil, fmt.Errorf("%s: %w", manifestPath, err)
		}
		if err := loadManifestMigrations(&manifest); err != nil {
			return nil, fmt.Errorf("%s: %w", manifestPath, err)
		}
		out = append(out, manifest)
		logging.Debugf("plugin manifest loaded plugin_id=%s dir=%s backend=%s frontend=%t themes=%d migrations=%d", manifest.ID, manifest.Dir, manifestBackendKind(manifest), manifest.Frontend != nil, len(manifest.Themes), len(manifest.Migrations))
	}
	return out, nil
}

// DefinitionsFromManifests converts runtime manifests into admin-visible plugin
// definitions that can be persisted alongside compiled plugins.
func DefinitionsFromManifests(manifests []Manifest) []Definition {
	out := make([]Definition, 0, len(manifests))
	for _, manifest := range manifests {
		out = append(out, Definition{
			ID:               manifest.ID,
			Name:             firstNonEmpty(manifest.Name, manifest.ID),
			Description:      strings.TrimSpace(manifest.Description),
			EnabledByDefault: manifest.EnabledByDefault,
			Heavy:            manifest.Heavy,
			Experimental:     manifest.Experimental,
		})
	}
	return out
}

// MigrationsFromManifests returns file-backed plugin migrations discovered
// while scanning plugin folders. Empty scope means all scopes.
func MigrationsFromManifests(manifests []Manifest, scope string) []Migration {
	scope = strings.TrimSpace(scope)
	var out []Migration
	for _, manifest := range manifests {
		for _, migration := range manifest.Migrations {
			if scope == "" || migration.Scope == scope {
				out = append(out, migration)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PluginID != out[j].PluginID {
			return out[i].PluginID < out[j].PluginID
		}
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func validateManifest(manifest Manifest) error {
	if !validManifestID(manifest.ID) {
		return fmt.Errorf("invalid plugin id %q", manifest.ID)
	}
	for _, theme := range manifest.Themes {
		if !validManifestID(theme.ID) {
			return fmt.Errorf("invalid theme id %q", theme.ID)
		}
		if strings.TrimSpace(theme.Name) == "" {
			return fmt.Errorf("theme %q has no name", theme.ID)
		}
		if !safeRelativeAssetPath(theme.CSS) {
			return fmt.Errorf("theme %q has invalid css path", theme.ID)
		}
		if _, err := os.Stat(filepath.Join(manifest.Dir, filepath.FromSlash(theme.CSS))); err != nil {
			return fmt.Errorf("theme %q css: %w", theme.ID, err)
		}
	}
	if manifest.Frontend != nil {
		if !safeRelativeAssetPath(manifest.Frontend.Module) {
			return fmt.Errorf("invalid frontend module path")
		}
		if manifest.Frontend.CSS != "" && !safeRelativeAssetPath(manifest.Frontend.CSS) {
			return fmt.Errorf("invalid frontend css path")
		}
	}
	if manifest.Backend != nil && manifest.Backend.Binary != "" && !safeRelativeAssetPath(manifest.Backend.Binary) {
		return fmt.Errorf("invalid backend binary path")
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func manifestBackendKind(manifest Manifest) string {
	if manifest.Backend == nil {
		return ""
	}
	return strings.TrimSpace(manifest.Backend.Kind)
}

func validManifestID(value string) bool {
	return manifestIDRE.MatchString(strings.TrimSpace(value))
}

func safeRelativeAssetPath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
