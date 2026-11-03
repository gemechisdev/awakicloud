package exim

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gemechischala/awaki/internal/mcp"
	"github.com/gemechischala/awaki/internal/permission"
	"github.com/gemechischala/awaki/internal/project"
	"github.com/gemechischala/awaki/internal/providers"
	"github.com/gemechischala/awaki/internal/scheduler"
	"github.com/gemechischala/awaki/internal/store"
)

// Source gathers live data from the running app for export.
type Source struct {
	Store     store.Store
	Providers *providers.Store
	MCP       *mcp.Store
	Projects  *project.Manager
	Scheduler *scheduler.Scheduler
	PermStore *permission.Store
	AppVersion string
}

// Export builds a ZIP archive according to sel and returns the bytes + summary.
func Export(src Source, sel Selection) ([]byte, Result, error) {
	res := Result{
		Counts: map[string]int{},
	}
	if len(sel.Categories) == 0 {
		return nil, res, fmt.Errorf("no categories selected")
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeJSON := func(name string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	writeBytes := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}

	man := Manifest{
		Format:     FormatName,
		Version:    FormatVersion,
		CreatedAt:  time.Now().UTC(),
		AppVersion: src.AppVersion,
		Categories: append([]string(nil), sel.Categories...),
		Items:      map[string][]string{},
		Counts:     map[string]int{},
		Notes:      "May contain API keys, OAuth tokens, and personal data. Treat as secret.",
	}
	hasSecrets := false

	// --- providers ---
	if sel.Has(CatProviders) && src.Providers != nil {
		cfg := src.Providers.Snapshot()
		filter := sel.ItemFilter(CatProviders)
		if filter != nil {
			var ps []providers.Provider
			for _, p := range cfg.Providers {
				if filter[p.ID] || filter[p.Name] {
					ps = append(ps, p)
				}
			}
			cfg.Providers = ps
			var ms []providers.Model
			for _, m := range cfg.Models {
				if filter[m.ProviderID] { // keep models of selected providers
					ms = append(ms, m)
				}
			}
			cfg.Models = ms
		}
		for _, p := range cfg.Providers {
			if p.APIKey != "" {
				hasSecrets = true
				break
			}
		}
		if err := writeJSON("providers.json", cfg); err != nil {
			return nil, res, err
		}
		n := len(cfg.Providers)
		man.Counts[CatProviders] = n
		res.Counts[CatProviders] = n
		if filter != nil {
			ids := make([]string, 0, n)
			for _, p := range cfg.Providers {
				ids = append(ids, p.ID)
			}
			man.Items[CatProviders] = ids
		}
	}

	// --- mcp ---
	if sel.Has(CatMCP) && src.MCP != nil {
		list, err := src.MCP.List()
		if err != nil {
			res.Warnings = append(res.Warnings, "mcp list: "+err.Error())
		} else {
			filter := sel.ItemFilter(CatMCP)
			var out []mcp.Server
			for _, s := range list {
				if filter != nil && !filter[s.Name] {
					continue
				}
				out = append(out, s)
				if s.OAuth != nil && (s.OAuth.AccessToken != "" || s.OAuth.RefreshToken != "" || s.OAuth.ClientSecret != "") {
					hasSecrets = true
				}
			}
			cfg := mcp.ConfigFile{Servers: out}
			if err := writeJSON("mcp.json", cfg); err != nil {
				return nil, res, err
			}
			man.Counts[CatMCP] = len(out)
			res.Counts[CatMCP] = len(out)
			if filter != nil {
				names := make([]string, 0, len(out))
				for _, s := range out {
					names = append(names, s.Name)
				}
				man.Items[CatMCP] = names
			}
		}
	}

	// --- skills ---
	if sel.Has(CatSkills) && src.Store != nil {
		skills, err := src.Store.LoadSkills()
		if err != nil {
			res.Warnings = append(res.Warnings, "skills: "+err.Error())
		} else {
			filter := sel.ItemFilter(CatSkills)
			count := 0
			var names []string
			for _, sk := range skills {
				if filter != nil && !filter[sk.Name] {
					continue
				}
				body, _ := src.Store.ReadSkill(sk.Name)
				se := SkillExport{
					Name:        sk.Name,
					Description: sk.Description,
					Body:        body,
				}
				// Prefer on-disk package if present
				pkgPath := filepath.Join(src.Store.SkillsDir(), sk.Name)
				if st, err := os.Stat(pkgPath); err == nil && st.IsDir() {
					// zip the skill directory
					if data, err := zipDir(pkgPath); err == nil && len(data) > 0 {
						_ = writeBytes(fmt.Sprintf("skills/%s/package.zip", sk.Name), data)
					}
				}
				if err := writeJSON(fmt.Sprintf("skills/%s/skill.json", sk.Name), se); err != nil {
					res.Warnings = append(res.Warnings, "skill "+sk.Name+": "+err.Error())
					continue
				}
				count++
				names = append(names, sk.Name)
			}
			man.Counts[CatSkills] = count
			res.Counts[CatSkills] = count
			if filter != nil {
				man.Items[CatSkills] = names
			}
		}
	}

	// --- identity ---
	if sel.Has(CatIdentity) && src.Store != nil {
		soul := src.Store.ReadSoul()
		user := src.Store.ReadUser()
		id := map[string]string{"soul": soul, "user": user}
		if err := writeJSON("identity.json", id); err != nil {
			return nil, res, err
		}
		n := 0
		if soul != "" {
			n++
		}
		if user != "" {
			n++
		}
		man.Counts[CatIdentity] = n
		res.Counts[CatIdentity] = n
	}

	// --- memory ---
	if sel.Has(CatMemory) && src.Store != nil {
		files, err := src.Store.ListMemory()
		if err != nil {
			res.Warnings = append(res.Warnings, "memory: "+err.Error())
		} else {
			filter := sel.ItemFilter(CatMemory)
			count := 0
			var names []string
			for _, f := range files {
				if filter != nil && !filter[f.Name] {
					continue
				}
				content := f.Content
				if content == "" {
					content, _ = src.Store.ReadMemory(f.Name)
				}
				if err := writeBytes("memory/"+f.Name, []byte(content)); err != nil {
					res.Warnings = append(res.Warnings, "memory "+f.Name+": "+err.Error())
					continue
				}
				count++
				names = append(names, f.Name)
			}
			man.Counts[CatMemory] = count
			res.Counts[CatMemory] = count
			if filter != nil {
				man.Items[CatMemory] = names
			}
		}
	}

	// --- schedules ---
	if sel.Has(CatSchedules) && src.Scheduler != nil {
		// Scheduler exposes List; fall back to reading config if needed.
		tasks := src.Scheduler.List()
		data, err := json.MarshalIndent(map[string]any{"tasks": tasks}, "", "  ")
		if err != nil {
			res.Warnings = append(res.Warnings, "schedules marshal: "+err.Error())
		} else {
			if err := writeBytes("schedules.json", data); err != nil {
				return nil, res, err
			}
			man.Counts[CatSchedules] = len(tasks)
			res.Counts[CatSchedules] = len(tasks)
		}
	}

	// --- permission modes ---
	if sel.Has(CatPermission) && src.PermStore != nil {
		// permission.Store has no public Snapshot; read the file if possible.
		// We use a best-effort dump via reflection-free path: List is not available,
		// so we export via the known path helper if present, else skip with warning.
		if data, err := dumpPermissionJSON(src.PermStore); err != nil {
			res.Warnings = append(res.Warnings, "permission_modes: "+err.Error())
		} else if len(data) > 0 {
			if err := writeBytes("permission_modes.json", data); err != nil {
				return nil, res, err
			}
			man.Counts[CatPermission] = 1
			res.Counts[CatPermission] = 1
		}
	}

	// --- chats ---
	if sel.Has(CatChats) && src.Store != nil {
		list, err := src.Store.ListChats()
		if err != nil {
			res.Warnings = append(res.Warnings, "chats list: "+err.Error())
		} else {
			filter := sel.ItemFilter(CatChats)
			count := 0
			var ids []string
			for _, info := range list {
				if filter != nil && !filter[info.ID] {
					continue
				}
				ch, err := src.Store.LoadChat(info.ID)
				if err != nil {
					res.Warnings = append(res.Warnings, "chat "+info.ID+": "+err.Error())
					continue
				}
				// strip non-serialisable fields
				meta := *ch
				ce := ChatExport{
					Meta:     meta,
					Messages: make([]any, 0, len(ch.Messages())),
				}
				for _, t := range ch.Messages() {
					ce.Messages = append(ce.Messages, t)
				}
				if err := writeJSON(fmt.Sprintf("chats/%s.json", info.ID), ce); err != nil {
					res.Warnings = append(res.Warnings, "chat write "+info.ID+": "+err.Error())
					continue
				}
				count++
				ids = append(ids, info.ID)
			}
			man.Counts[CatChats] = count
			res.Counts[CatChats] = count
			if filter != nil {
				man.Items[CatChats] = ids
			}
		}
	}

	// --- projects ---
	if sel.Has(CatProjects) && src.Projects != nil {
		metas, err := src.Projects.ListLocal()
		if err != nil {
			res.Warnings = append(res.Warnings, "projects: "+err.Error())
		} else {
			filter := sel.ItemFilter(CatProjects)
			count := 0
			var slugs []string
			for _, m := range metas {
				if filter != nil && !filter[m.Slug] && !filter[m.Name] {
					continue
				}
				pe := ProjectExport{Meta: MetaLite{
					ID: m.ID, Name: m.Name, Slug: m.Slug, ChatID: m.ChatID,
					ContentHash: m.ContentHash, FileCount: m.FileCount,
					TotalSize: m.TotalSize, UpdatedAt: m.UpdatedAt,
				}}
				if err := writeJSON(fmt.Sprintf("projects/%s/meta.json", m.Slug), pe); err != nil {
					res.Warnings = append(res.Warnings, "project meta "+m.Slug+": "+err.Error())
					continue
				}
				dir := src.Projects.ProjectDir(m.Slug)
				if data, _, _, _, err := project.ZipTree(dir); err == nil && len(data) > 0 {
					_ = writeBytes(fmt.Sprintf("projects/%s/tree.zip", m.Slug), data)
				}
				count++
				slugs = append(slugs, m.Slug)
			}
			man.Counts[CatProjects] = count
			res.Counts[CatProjects] = count
			if filter != nil {
				man.Items[CatProjects] = slugs
			}
		}
	}

	man.HasSecrets = hasSecrets
	if err := writeJSON("manifest.json", man); err != nil {
		return nil, res, err
	}
	if err := zw.Close(); err != nil {
		return nil, res, err
	}
	res.Manifest = man
	return buf.Bytes(), res, nil
}

func zipDir(root string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			_, err := zw.Create(rel + "/")
			return err
		}
		w, err := zw.Create(rel)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func dumpPermissionJSON(ps *permission.Store) ([]byte, error) {
	return ps.ExportJSON()
}
