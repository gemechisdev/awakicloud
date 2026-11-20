package filemgr

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is one row of a directory listing.
type Entry struct {
	Name    string
	Dir     bool
	Symlink bool
	// Outside marks a symlink whose target leaves the workspace; it is shown
	// but never gets a button.
	Outside bool
	Size    int64
}

// Listing is a sorted, capped directory listing.
type Listing struct {
	Entries   []Entry
	Total     int  // entries on disk
	Truncated bool // Total > len(Entries)
}

// ListDir lists abs: directories first, then files, case-insensitive by name
// (same order as the list_files tool). Dotfiles are included.
func ListDir(root, abs string) (Listing, error) {
	des, err := os.ReadDir(abs)
	if err != nil {
		return Listing{}, err
	}
	realRoot, rerr := filepath.EvalSymlinks(root)
	if rerr != nil {
		realRoot = root
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		e := Entry{Name: de.Name(), Dir: de.IsDir()}
		full := filepath.Join(abs, de.Name())
		if de.Type()&os.ModeSymlink != 0 {
			e.Symlink = true
			real, err := filepath.EvalSymlinks(full)
			switch {
			case err != nil:
				// dangling link: shown, not openable
				e.Outside = true
			case !Within(realRoot, real):
				e.Outside = true
			default:
				if st, err := os.Stat(full); err == nil {
					e.Dir = st.IsDir()
					if !e.Dir {
						e.Size = st.Size()
					}
				}
			}
		} else if !e.Dir {
			if info, err := de.Info(); err == nil {
				e.Size = info.Size()
			}
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].Name < out[j].Name
	})
	l := Listing{Entries: out, Total: len(out)}
	if len(out) > MaxList {
		l.Entries = out[:MaxList]
		l.Truncated = true
	}
	return l, nil
}

// HumanSize formats a byte count like "14.2 KB".
func HumanSize(b int64) string {
	if b < 0 {
		return "?"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(b)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", b)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
