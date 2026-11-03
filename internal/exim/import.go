package exim

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// Import applies the archive according to sel.
//
// Merge vs overwrite (applies to every category):
//
//	Merge (sel.Overwrite == false):
//	  - Insert entities whose key is missing locally (exact exported IDs/names/slugs).
//	  - Leave existing local entities unchanged.
//	  - Never delete local-only entities that are not in the package (union).
//	  - Related data accumulates (e.g. chats under a project, models under a provider).
//
//	Overwrite (sel.Overwrite == true):
//	  - Replace entities that share the same key with the imported version.
//	  - Still never delete local-only entities absent from the package.
//
// Categories absent from the archive are skipped with a warning.
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

	// providers (and models) — preserve original IDs so model→provider links stay valid
	if sel.Has(CatProviders) && tgt.Providers != nil {
		var cfg providers.ConfigFile
		if err := readJSON("providers.json", &cfg); err != nil {
			res.Warnings = append(res.Warnings, "providers: "+err.Error())
		} else {
			filter := sel.ItemFilter(CatProviders)
			if filter != nil {
				var ps []providers.Provider
				for _, p := range cfg.Providers {
					if filter[p.ID] || filter[p.Name] {
						ps = append(ps, p)
					}
				}
				cfg.Providers = ps
				keep := map[string]bool{}
				for _, p := range ps {
					keep[p.ID] = true
				}
				var ms []providers.Model
				for _, m := range cfg.Models {
					if keep[m.ProviderID] {
						ms = append(ms, m)
					}
				}
				cfg.Models = ms
			}
			pn, mn, err := tgt.Providers.ImportConfig(cfg, sel.Overwrite)
			if err != nil {
				res.Warnings = append(res.Warnings, "providers: "+err.Error())
			}
			res.Counts[CatProviders] = pn
			if mn > 0 || len(cfg.Models) > 0 {
				// Informational note (not a failure) so the UI shows model counts
				res.Warnings = append(res.Warnings, fmt.Sprintf("models: %d applied (%d in package)", mn, len(cfg.Models)))
			}
			if mn < len(cfg.Models) {
				skipped := len(cfg.Models) - mn
				if skipped > 0 && !sel.Overwrite {
					res.Warnings = append(res.Warnings, fmt.Sprintf("%d models skipped (already present; use Overwrite to replace)", skipped))
				}
			}
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

	// projects FIRST (exact slug + id) so chat links resolve; chats are unioned by id below
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
			meta := project.Meta{
				ID:          pe.Meta.ID,
				Name:        pe.Meta.Name,
				Slug:        pe.Meta.Slug,
				ChatID:      pe.Meta.ChatID,
				ContentHash: pe.Meta.ContentHash,
				FileCount:   pe.Meta.FileCount,
				TotalSize:   pe.Meta.TotalSize,
				UpdatedAt:   pe.Meta.UpdatedAt,
			}
			if meta.Slug == "" {
				meta.Slug = slug
			}
			effective, changed, err := tgt.Projects.ImportProject(meta, sel.Overwrite)
			if err != nil {
				res.Warnings = append(res.Warnings, "project "+meta.Slug+": "+err.Error())
				continue
			}
			// Workspace tree: overwrite replaces files; merge only adds missing paths
			treeName := fmt.Sprintf("projects/%s/tree.zip", slug)
			treeChanged := false
			if treeData, err := readBytes(treeName); err == nil && len(treeData) > 0 {
				dir := tgt.Projects.ProjectDir(effective.Slug)
				_ = os.MkdirAll(dir, 0o755)
				var zerr error
				if sel.Overwrite {
					zerr = unzipTo(treeData, dir)
				} else {
					treeChanged, zerr = unzipToMerge(treeData, dir)
				}
				if zerr != nil {
					res.Warnings = append(res.Warnings, "project tree "+effective.Slug+": "+zerr.Error())
				}
			}
			if changed || treeChanged {
				n++
			} else if !sel.Overwrite {
				// Project already present — still counts as available for chat union
				n++
			}
			_ = effective
		}
		res.Counts[CatProjects] = n
	}

	// chats — exact IDs; merge adds missing chats (union under same project_slug);
	// overwrite replaces matching chat meta+messages. Local-only chats are never deleted.
	if sel.Has(CatChats) && tgt.Store != nil {
		filter := sel.ItemFilter(CatChats)
		n := 0
		for name, f := range files {
			if !strings.HasPrefix(name, "chats/") || !strings.HasSuffix(name, ".json") {
				continue
			}
			fileID := strings.TrimSuffix(strings.TrimPrefix(name, "chats/"), ".json")
			if filter != nil && !filter[fileID] {
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
				res.Warnings = append(res.Warnings, "chat "+fileID+": "+err.Error())
				continue
			}
			metaBytes, _ := json.Marshal(ce.Meta)
			var ch store.Chat
			if err := json.Unmarshal(metaBytes, &ch); err != nil {
				res.Warnings = append(res.Warnings, "chat meta "+fileID+": "+err.Error())
				continue
			}
			if ch.ID == "" {
				ch.ID = fileID
			}
			if existing, err := tgt.Store.LoadChat(ch.ID); err == nil && existing != nil && !sel.Overwrite {
				continue // merge: keep local chat as-is
			}
			// Point project_id at the local project for this slug (union under one project)
			if ch.ProjectSlug != "" && tgt.Projects != nil {
				if pm, err := tgt.Projects.ReadMeta(ch.ProjectSlug); err == nil {
					ch.ProjectID = pm.ID
				}
			}
			ch.SetPersister(tgt.Store)
			if err := tgt.Store.SaveChat(&ch); err != nil {
				res.Warnings = append(res.Warnings, "chat save "+ch.ID+": "+err.Error())
				continue
			}
			var turns []store.Turn
			for _, m := range ce.Messages {
				b, _ := json.Marshal(m)
				var turn store.Turn
				if json.Unmarshal(b, &turn) == nil {
					turns = append(turns, turn)
				}
			}
			if len(turns) > 0 || sel.Overwrite {
				if err := tgt.Store.ReplaceMessages(&ch, turns); err != nil {
					res.Warnings = append(res.Warnings, "chat messages "+ch.ID+": "+err.Error())
				}
			}
			n++
		}
		res.Counts[CatChats] = n
	}

	return res, nil
}

func unzipTo(data []byte, dest string) error {
	_, err := unzipExtract(data, dest, true)
	return err
}

// unzipToMerge writes only files that do not already exist under dest (union).
func unzipToMerge(data []byte, dest string) (added bool, err error) {
	return unzipExtract(data, dest, false)
}

func unzipExtract(data []byte, dest string, overwrite bool) (wrote bool, err error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return false, err
	}
	destClean := filepath.Clean(dest)
	for _, f := range zr.File {
		target := filepath.Join(dest, f.Name)
		clean := filepath.Clean(target)
		if clean != destClean && !strings.HasPrefix(clean, destClean+string(os.PathSeparator)) {
			continue // zip slip
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0o755)
			continue
		}
		if !overwrite {
			if _, statErr := os.Stat(target); statErr == nil {
				continue // keep local file
			}
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
		wrote = true
	}
	return wrote, nil
}
