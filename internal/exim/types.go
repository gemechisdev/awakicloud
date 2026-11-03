// Package exim provides selective export/import of Awaki application data.
// Format is a ZIP archive containing a versioned manifest and per-category
// JSON / file trees. Secrets (API keys, OAuth tokens) are included when present;
// the exporter always warns the operator.
package exim

import "time"

const (
	FormatName    = "awaki-export"
	FormatVersion = 1
)

// Category identifiers used in the manifest and selection UI.
const (
	CatProviders  = "providers"
	CatMCP        = "mcp"
	CatSkills     = "skills"
	CatProjects   = "projects"
	CatChats      = "chats"
	CatMemory     = "memory"
	CatIdentity   = "identity"
	CatSchedules  = "schedules"
	CatPermission = "permission_modes"
)

// AllCategories is the ordered list shown in UIs.
var AllCategories = []string{
	CatProviders,
	CatMCP,
	CatSkills,
	CatIdentity,
	CatMemory,
	CatSchedules,
	CatPermission,
	CatProjects,
	CatChats,
}

// CategoryLabel returns a short human label for a category key.
func CategoryLabel(c string) string {
	switch c {
	case CatProviders:
		return "Providers & models"
	case CatMCP:
		return "MCP servers"
	case CatSkills:
		return "Skills"
	case CatProjects:
		return "Projects"
	case CatChats:
		return "Chats"
	case CatMemory:
		return "Memory files"
	case CatIdentity:
		return "Soul & user"
	case CatSchedules:
		return "Schedules"
	case CatPermission:
		return "Permission modes"
	default:
		return c
	}
}

// Manifest is the root descriptor written as manifest.json inside the ZIP.
type Manifest struct {
	Format     string            `json:"format"`
	Version    int               `json:"version"`
	CreatedAt  time.Time         `json:"created_at"`
	AppVersion string            `json:"app_version,omitempty"`
	Categories []string          `json:"categories"` // included top-level categories
	Items      map[string][]string `json:"items,omitempty"` // optional name/id filters per category
	Counts     map[string]int    `json:"counts,omitempty"`
	HasSecrets bool              `json:"has_secrets"`
	Notes      string            `json:"notes,omitempty"`
}

// Selection describes what the operator wants to export or import.
type Selection struct {
	Categories []string            // empty = none
	Items      map[string][]string // if set for a category, only those names/ids
	Overwrite  bool                // import only: replace existing instead of merge/skip
}

func (s Selection) Has(cat string) bool {
	for _, c := range s.Categories {
		if c == cat {
			return true
		}
	}
	return false
}

func (s Selection) ItemFilter(cat string) map[string]bool {
	names, ok := s.Items[cat]
	if !ok || len(names) == 0 {
		return nil // no filter → all
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// ChatExport is one chat with its turns (messages) for archive.
type ChatExport struct {
	Meta     any       `json:"meta"` // store.Chat without persister
	Messages []any     `json:"messages"`
}

// ProjectExport holds metadata + optional workspace zip bytes (stored as sibling file).
type ProjectExport struct {
	Meta MetaLite `json:"meta"`
}

// MetaLite is a serialisable subset of project.Meta.
type MetaLite struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	ChatID      string    `json:"chat_id,omitempty"`
	ContentHash string    `json:"content_hash,omitempty"`
	FileCount   int       `json:"file_count,omitempty"`
	TotalSize   int64     `json:"total_size,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SkillExport stores skill body + optional package zip (as base64 or separate file).
type SkillExport struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	PackageHash string `json:"package_hash,omitempty"`
	// PackageZip is written as skills/<name>/package.zip when non-empty.
}

// Result summarises an export or import operation.
type Result struct {
	Manifest Manifest
	Counts   map[string]int
	Warnings []string
	Errors   []string
}
