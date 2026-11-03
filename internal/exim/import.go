package exim

import (
	"context"
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gemechischala/awaki/internal/mcp"
	"github.com/gemechischala/awaki/internal/permission"
	"github.com/gemechischala/awaki/internal/project"
	"github.com/gemechischala/awaki/internal/providers"
	"github.com/gemechischala/awaki/internal/scheduler"
	"github.com/gemechischala/awaki/internal/store"
)

// Target receives imported data.
type Target struct {
	Store     store.Store
	Providers *providers.Store
	MCP       *mcp.Store
	Projects  *project.Manager
	Scheduler *scheduler.Scheduler
	PermStore *permission.Store
}

// Inspect opens a ZIP and returns the manifest without applying changes.
func Inspect(data []byte) (Manifest, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Manifest{}, fmt.Errorf("not a valid zip: %w", err)
	}
	f, err := zr.Open("manifest.json")
	if err != nil {
		return Manifest{}, fmt.Errorf("missing manifest.json (not an awaki export?)")
	}
	defer f.Close()
	var man Manifest
	if err := json.NewDecoder(f).Decode(&man); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if man.Format != FormatName {
		return Manifest{}, fmt.Errorf("unsupported format %q", man.Format)
	}
	return man, nil
}

// Import applies the archive according to sel. Categories in sel that are
// absent from the archive are skipped with a warning.
func Import(data []byte, tgt Target, sel Selection) (Result, error) {
	res := Result{Counts: map[string]int{}}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return res, fmt.Errorf("not a valid zip: %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	readJSON := func(name string, v any) error {
		f, ok := files[name]
		if !ok {
			return fmt.Errorf("missing %s", name)
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return json.NewDecoder(rc).Decode(v)
	}
	readBytes := func(name string) ([]byte, error) {
		f, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("missing %s", name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}

	man, err := Inspect(data)
	if err != nil {
		return res, err
	}
	res.Manifest = man

	if len(sel.Categories) == 0 {
		// default: import everything present in the archive
		sel.Categories = append([]string(nil), man.Categories...)
	}

	// providers
	if sel.Has(CatProviders) && tgt.Providers != nil {
		var cfg providers.ConfigFile
		if err := readJSON("providers.json", &cfg); err != nil {
			res.Warnings = append(res.Warnings, "providers: "+err.Error())
		} else {
			filter := sel.ItemFilter(CatProviders)
			n := 0
			for _, p := range cfg.Providers {
				if filter != nil && !filter[p.ID] && !filter[p.Name] {
					continue
				}
				existing, ok := tgt.Providers.GetProvider(p.ID)
				if ok && !sel.Overwrite {
					// still allow key update if empty?
					_ = existing
					continue
				}
				// Prefer Upsert-style via Add then Update, or direct if available.
				if !ok {
					np, err := tgt.Providers.AddProvider(p.Name, p.BaseURL, p.Protocol, p.APIKey)
					if err != nil {
						res.Warnings = append(res.Warnings, "provider "+p.Name+": "+err.Error())
						continue
					}
					// keep original ID if possible by re-writing snapshot is hard; use new id mapping
					_ = np
				} else if sel.Overwrite {
					_, _ = tgt.Providers.UpdateProvider(p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.Protocol)
				}
				n++
			}
			// models
			for _, m := range cfg.Models {
				if filter != nil {
					// filter by provider presence
					found := false
					for _, p := range cfg.Providers {
						if (filter[p.ID] || filter[p.Name]) && p.ID == m.ProviderID {
							found = true
							break
						}
					}
					if !found && len(filter) > 0 {
						continue
					}
				}
				if _, ok := tgt.Providers.GetModel(m.ID); ok && !sel.Overwrite {
					continue
				}
				_, err := tgt.Providers.AddModel(m.ProviderID, m.ModelID, m.DisplayName, m.ContextWindow)
				if err != nil {
					// may fail if provider id changed; try by name match later
					res.Warnings = append(res.Warnings, "model "+m.DisplayName+": "+err.Error())
					continue
				}
			}
			if cfg.ActiveModelID != "" {
				_ = tgt.Providers.SetActiveModel(cfg.ActiveModelID)
			}
			res.Counts[CatProviders] = n
		}
	}

	// mcp
	if sel.Has(CatMCP) && tgt.MCP != nil {
		var cfg mcp.ConfigFile
		if err := readJSON("mcp.json", &cfg); err != nil {
			res.Warnings = append(res.Warnings, "mcp: "+err.Error())
		} else {
			filter := sel.ItemFilter(CatMCP)
			n := 0
			for _, srv := range cfg.Servers {
				if filter != nil && !filter[srv.Name] {
					continue
				}
				_, exists, _ := tgt.MCP.Get(srv.Name)
				if exists && !sel.Overwrite {
					continue
				}
				if err := tgt.MCP.Upsert(srv); err != nil {
					res.Warnings = append(res.Warnings, "mcp "+srv.Name+": "+err.Error())
					continue
				}
				n++
			}
			res.Counts[CatMCP] = n
		}
	}

	// skills
	if sel.Has(CatSkills) && tgt.Store != nil {
		filter := sel.ItemFilter(CatSkills)
		n := 0
		for name, f := range files {
			if !strings.HasPrefix(name, "skills/") || !strings.HasSuffix(name, "/skill.json") {
				continue
			}
			parts := strings.Split(name, "/")
			if len(parts) < 3 {
				continue
			}
			skillName := parts[1]
			if filter != nil && !filter[skillName] {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				continue
			}
			var se SkillExport
			err = json.NewDecoder(rc).Decode(&se)
			rc.Close()
			if err != nil {
				res.Warnings = append(res.Warnings, "skill decode "+skillName+": "+err.Error())
				continue
			}
			existing, _ := tgt.Store.ReadSkill(skillName)
			if existing != "" && !sel.Overwrite {
				continue
			}
			if existing == "" {
				_, err = tgt.Store.CreateSkill(se.Name, se.Description, se.Body)
			} else {
				err = tgt.Store.UpdateSkill(se.Name, se.Body)
			}
			if err != nil {
				res.Warnings = append(res.Warnings, "skill "+skillName+": "+err.Error())
				continue
			}
			// optional package.zip
			pkgName := fmt.Sprintf("skills/%s/package.zip", skillName)
			if pf, ok := files[pkgName]; ok {
				if data, err := func() ([]byte, error) {
					rc, err := pf.Open()
					if err != nil {
						return nil, err
					}
					defer rc.Close()
					return io.ReadAll(rc)
				}(); err == nil && len(data) > 0 {
					dir := filepath.Join(tgt.Store.SkillsDir(), skillName)
					_ = os.MkdirAll(dir, 0o755)
					_ = unzipTo(data, dir)
					type skillPackager interface {
						SyncSkillPackage(name string) (string, bool, error)
					}
					if sp, ok := tgt.Store.(skillPackager); ok {
						_, _, _ = sp.SyncSkillPackage(skillName)
					}
				}
			}
			n++
		}
		res.Counts[CatSkills] = n
	}

	// identity
	if sel.Has(CatIdentity) && tgt.Store != nil {
		var id map[string]string
		if err := readJSON("identity.json", &id); err != nil {
			res.Warnings = append(res.Warnings, "identity: "+err.Error())
		} else {
			n := 0
			if soul, ok := id["soul"]; ok && soul != "" {
				if sel.Overwrite || tgt.Store.ReadSoul() == "" {
					_ = tgt.Store.WriteSoul(soul)
					n++
				}
			}
			if user, ok := id["user"]; ok && user != "" {
				if sel.Overwrite || tgt.Store.ReadUser() == "" {
					_ = tgt.Store.WriteUser(user)
					n++
				}
			}
			res.Counts[CatIdentity] = n
		}
	}

	// memory
	if sel.Has(CatMemory) && tgt.Store != nil {
		filter := sel.ItemFilter(CatMemory)
		n := 0
		for name, f := range files {
			if !strings.HasPrefix(name, "memory/") || strings.HasSuffix(name, "/") {
				continue
			}
			fname := strings.TrimPrefix(name, "memory/")
			if filter != nil && !filter[fname] {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				continue
			}
			content, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				continue
			}
			if !sel.Overwrite {
				if existing, err := tgt.Store.ReadMemory(fname); err == nil && existing != "" {
					continue
				}
			}
			if err := tgt.Store.WriteMemory(fname, string(content)); err != nil {
				res.Warnings = append(res.Warnings, "memory "+fname+": "+err.Error())
				continue
			}
			n++
		}
		res.Counts[CatMemory] = n
	}

	// schedules
	if sel.Has(CatSchedules) && tgt.Scheduler != nil {
		raw, err := readBytes("schedules.json")
		if err != nil {
			res.Warnings = append(res.Warnings, "schedules: "+err.Error())
		} else {
			var payload struct {
				Tasks []scheduler.TaskInfo `json:"tasks"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				res.Warnings = append(res.Warnings, "schedules parse: "+err.Error())
			} else {
				n := 0
				for _, t := range payload.Tasks {
					// skip if already exists (by id) unless overwrite
					exists := false
					for _, cur := range tgt.Scheduler.List() {
						if cur.ID == t.ID {
							exists = true
							break
						}
					}
					if exists && !sel.Overwrite {
						continue
					}
					if exists {
						_ = tgt.Scheduler.Remove(t.ID)
					}
					if err := tgt.Scheduler.AddWithOptions(t.ID, t.Schedule, t.Command, t.Skill, t.Label, t.MaxRuns); err != nil {
						res.Warnings = append(res.Warnings, "schedule "+t.ID+": "+err.Error())
						continue
					}
					if t.Paused {
						_ = tgt.Scheduler.Pause(t.ID, true)
					}
					n++
				}
				res.Counts[CatSchedules] = n
			}
		}
	}

	// permission modes
	if sel.Has(CatPermission) && tgt.PermStore != nil {
		raw, err := readBytes("permission_modes.json")
		if err != nil {
			res.Warnings = append(res.Warnings, "permission_modes: "+err.Error())
		} else {
			if err := tgt.PermStore.ImportJSON(raw, sel.Overwrite); err != nil {
				res.Warnings = append(res.Warnings, "permission_modes: "+err.Error())
			} else {
				res.Counts[CatPermission] = 1
			}
		}
	}

	// chats
	if sel.Has(CatChats) && tgt.Store != nil {
		filter := sel.ItemFilter(CatChats)
		n := 0
		for name, f := range files {
			if !strings.HasPrefix(name, "chats/") || !strings.HasSuffix(name, ".json") {
				continue
			}
			id := strings.TrimSuffix(strings.TrimPrefix(name, "chats/"), ".json")
			if filter != nil && !filter[id] {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				continue
			}
			var ce ChatExport
			err = json.NewDecoder(rc).Decode(&ce)
			rc.Close()
			if err != nil {
				res.Warnings = append(res.Warnings, "chat "+id+": "+err.Error())
				continue
			}
			// decode meta into store.Chat
			metaBytes, _ := json.Marshal(ce.Meta)
			var ch store.Chat
			if err := json.Unmarshal(metaBytes, &ch); err != nil {
				res.Warnings = append(res.Warnings, "chat meta "+id+": "+err.Error())
				continue
			}
			if existing, err := tgt.Store.LoadChat(ch.ID); err == nil && existing != nil && !sel.Overwrite {
				continue
			}
			ch.SetPersister(tgt.Store)
			if err := tgt.Store.SaveChat(&ch); err != nil {
				res.Warnings = append(res.Warnings, "chat save "+id+": "+err.Error())
				continue
			}
			// messages
			var turns []store.Turn
			for _, m := range ce.Messages {
				b, _ := json.Marshal(m)
				var t store.Turn
				if json.Unmarshal(b, &t) == nil {
					turns = append(turns, t)
				}
			}
			if len(turns) > 0 {
				_ = tgt.Store.ReplaceMessages(&ch, turns)
			}
			n++
		}
		res.Counts[CatChats] = n
	}

	// projects
	if sel.Has(CatProjects) && tgt.Projects != nil {
		filter := sel.ItemFilter(CatProjects)
		n := 0
		for name := range files {
			if !strings.HasPrefix(name, "projects/") || !strings.HasSuffix(name, "/meta.json") {
				continue
			}
			parts := strings.Split(name, "/")
			if len(parts) < 3 {
				continue
			}
			slug := parts[1]
			if filter != nil && !filter[slug] {
				continue
			}
			var pe ProjectExport
			if err := readJSON(name, &pe); err != nil {
				res.Warnings = append(res.Warnings, "project "+slug+": "+err.Error())
				continue
			}
			exists, _, _ := tgt.Projects.ProjectExists(context.Background(), slug)
			if exists && !sel.Overwrite {
				continue
			}
			// create if missing
			if !exists {
				meta, err := tgt.Projects.Create(pe.Meta.Name, pe.Meta.ChatID)
				if err != nil {
					res.Warnings = append(res.Warnings, "project create "+slug+": "+err.Error())
					continue
				}
				slug = meta.Slug
			}
			// extract tree
			treeName := fmt.Sprintf("projects/%s/tree.zip", parts[1])
			if treeData, err := readBytes(treeName); err == nil && len(treeData) > 0 {
				dir := tgt.Projects.ProjectDir(slug)
				_ = os.MkdirAll(dir, 0o755)
				_ = unzipTo(treeData, dir)
			}
			n++
		}
		res.Counts[CatProjects] = n
	}

	return res, nil
}

func unzipTo(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		target := filepath.Join(dest, f.Name)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dest)+string(os.PathSeparator)) &&
			filepath.Clean(target) != filepath.Clean(dest) {
			continue // zip slip
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0o755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		rc, err := f.Open()
		if err != nil {
			continue
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			continue
		}
		_, _ = io.Copy(out, rc)
		out.Close()
		rc.Close()
	}
	return nil
}

// unused but keep time for future timestamps
var _ = time.Now
