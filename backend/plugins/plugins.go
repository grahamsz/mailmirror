// File overview: Compiled plugin registry and shared plugin schema types.

package plugins

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"sync"

	"rolltop/backend/logging"
)

const (
	BIMIBrandIcons       = "bimi_brand_icons"
	GravatarSenderIcons  = "gravatar_sender_icons"
	RemoteImageBlocklist = "remote_image_blocklist"
	TrustedImageSources  = "trusted_image_sources"
	AttachmentPreview    = "attachment_preview"
	LanguageSearch       = "language_search"
	OneClickUnsubscribe  = "one_click_unsubscribe"
	ClientSidePGP        = "client_side_pgp"
	MatrixTheme          = "matrix_theme"
)

const (
	ScopeSystem = "system"
	ScopeUser   = "user"
)

// Definition describes a compiled plugin and how it should appear in admin settings.
type Definition struct {
	ID               string
	Name             string
	Description      string
	EnabledByDefault bool
	Heavy            bool
	Experimental     bool
}

// Migration describes one plugin-owned schema change and checksum source.
type Migration struct {
	Scope         string
	PluginID      string
	ID            string
	EnsureColumns []MigrationColumn
	Statements    []string
	Apply         func(context.Context, *sql.Tx) error
}

// MigrationColumn is an idempotent column addition requested by a plugin
// migration. It is used by file-backed migrations because SQLite cannot
// reliably express ADD COLUMN IF NOT EXISTS across every deployed version.
type MigrationColumn struct {
	Table  string
	Column string
	DDL    string
}

var registry = struct {
	sync.RWMutex
	definitions map[string]Definition
	order       []string
	migrations  []Migration
	hooks       map[string][]any
}{definitions: map[string]Definition{}, hooks: map[string][]any{}}

// Register adds one statically compiled plugin package to the runtime registry.
// Plugin packages live under /plugins and call this from init so the main app can
// build them together without keeping implementation metadata in the core.
func Register(def Definition, migrations ...Migration) {
	def.ID = strings.TrimSpace(def.ID)
	if def.ID == "" {
		return
	}
	registry.Lock()
	defer registry.Unlock()
	if _, exists := registry.definitions[def.ID]; !exists {
		registry.order = append(registry.order, def.ID)
	}
	registry.definitions[def.ID] = def
	for _, migration := range migrations {
		migration.PluginID = strings.TrimSpace(migration.PluginID)
		if migration.PluginID == "" {
			migration.PluginID = def.ID
		}
		migration.ID = strings.TrimSpace(migration.ID)
		if migration.ID == "" {
			continue
		}
		migration.Scope = strings.TrimSpace(migration.Scope)
		if migration.Scope == "" {
			migration.Scope = ScopeSystem
		}
		registry.migrations = append(registry.migrations, migration)
	}
	logging.Debugf("plugin module registered plugin_id=%s migrations=%d enabled_by_default=%t heavy=%t", def.ID, len(migrations), def.EnabledByDefault, def.Heavy)
}

// RegisterHooks adds compiled plugin hook implementations without requiring the
// host packages to import concrete plugin implementation packages.
func RegisterHooks(pluginID string, hooks ...any) {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" || len(hooks) == 0 {
		return
	}
	registry.Lock()
	defer registry.Unlock()
	registry.hooks[pluginID] = append(registry.hooks[pluginID], hooks...)
}

// Hooks returns hook implementations registered for one compiled plugin.
func Hooks(pluginID string) []any {
	pluginID = strings.TrimSpace(pluginID)
	registry.RLock()
	defer registry.RUnlock()
	registered := registry.hooks[pluginID]
	out := make([]any, len(registered))
	copy(out, registered)
	return out
}

// All returns every compiled plugin definition in display order for admin settings and migration seeding.
func All() []Definition {
	registry.RLock()
	defer registry.RUnlock()
	out := make([]Definition, 0, len(registry.order))
	for _, id := range registry.order {
		out = append(out, registry.definitions[id])
	}
	return out
}

// Lookup returns one plugin definition by ID for enablement checks and plugin-specific routes.
func Lookup(id string) (Definition, bool) {
	id = strings.TrimSpace(id)
	registry.RLock()
	defer registry.RUnlock()
	def, ok := registry.definitions[id]
	return def, ok
}

// Migrations returns compiled plugin schema changes for one database scope.
func Migrations(scope string) []Migration {
	scope = strings.TrimSpace(scope)
	registry.RLock()
	defer registry.RUnlock()
	out := make([]Migration, 0, len(registry.migrations))
	for _, migration := range registry.migrations {
		if scope == "" || migration.Scope == scope {
			out = append(out, migration)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PluginID != out[j].PluginID {
			return out[i].PluginID < out[j].PluginID
		}
		return out[i].ID < out[j].ID
	})
	return out
}
