// Package browse lists directories for the dashboard's project picker: the
// subdirectories you might descend into, and the pairin configs you might add.
//
// It is deliberately not a general file browser. Everything that isn't a
// directory or a .pairinrc*.toml is left out, because the only reason to be
// looking is to find a config.
package browse

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Entry is one row in the picker.
type Entry struct {
	// Name is what to display: a directory with a trailing slash, or a
	// config's filename.
	Name string
	// Path is absolute.
	Path string

	IsParent bool
	IsDir    bool
	IsConfig bool

	// Project is the [project].name from a config, so the choice can be made
	// by project rather than by filename. Several projects keep more than one
	// config side by side, and the filenames alone don't distinguish them.
	Project string

	// Configs counts the pairin configs directly inside a directory, so it's
	// clear which ones are worth opening. -1 means "not counted".
	Configs int

	// Added is true when the config is already in the catalog. Pinned says
	// whether it is actually *shown* there — an unpinned, stopped project is
	// catalogued but invisible, and offering to add it is the right thing to
	// do rather than refusing because a record exists somewhere.
	Added  bool
	Pinned bool
}

// maxProbes bounds how many subdirectories are scanned for a config count.
// A directory of a few hundred entries costs a readdir each; past this the
// counts are simply omitted rather than made the user wait.
const maxProbes = 200

// skipDirs are never listed. Dot-directories are skipped too — pairin configs
// live at the top of a project, not inside its tooling.
var skipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"target":       true,
	"dist":         true,
}

// IsConfigName reports whether a filename looks like a pairin config.
// Covers `.pairinrc.toml` and variants such as `.pairinrc.localdev.toml`.
func IsConfigName(name string) bool {
	return strings.HasPrefix(name, ".pairinrc") && strings.HasSuffix(name, ".toml")
}

// Read lists dir. lookup reports, for a config path, whether it is already
// catalogued and whether it is pinned. It may be nil.
//
// Configs come first, then subdirectories, each alphabetical, with the parent
// directory at the top. That ordering puts the thing you came for above the
// places you'd go looking for it.
func Read(dir string, lookup func(string) (added, pinned bool)) ([]Entry, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	items, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}

	var configs, dirs []Entry
	probes := 0

	for _, it := range items {
		name := it.Name()

		if it.IsDir() {
			if strings.HasPrefix(name, ".") || skipDirs[name] {
				continue
			}
			e := Entry{
				Name:    name + string(filepath.Separator),
				Path:    filepath.Join(abs, name),
				IsDir:   true,
				Configs: -1,
			}
			if probes < maxProbes {
				e.Configs = CountConfigs(e.Path)
				probes++
			}
			dirs = append(dirs, e)
			continue
		}

		if !IsConfigName(name) {
			continue
		}
		path := filepath.Join(abs, name)
		e := Entry{
			Name:     name,
			Path:     path,
			IsConfig: true,
			Project:  ProjectName(path),
			Configs:  -1,
		}
		if lookup != nil {
			e.Added, e.Pinned = lookup(path)
		}
		configs = append(configs, e)
	}

	sort.Slice(configs, func(i, j int) bool { return configs[i].Name < configs[j].Name })
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })

	out := make([]Entry, 0, len(configs)+len(dirs)+1)
	if parent := filepath.Dir(abs); parent != abs {
		out = append(out, Entry{
			Name:     "../",
			Path:     parent,
			IsParent: true,
			IsDir:    true,
			Configs:  -1,
		})
	}
	out = append(out, configs...)
	return append(out, dirs...), nil
}

// CountConfigs counts pairin configs directly inside dir. An unreadable
// directory counts zero rather than failing — a permission error on one entry
// shouldn't stop the listing.
func CountConfigs(dir string) int {
	items, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, it := range items {
		if !it.IsDir() && IsConfigName(it.Name()) {
			n++
		}
	}
	return n
}

// ProjectName reads just the [project].name out of a config. It deliberately
// avoids config.Load: a config with an invalid service definition should still
// be listed with its name rather than vanishing from the picker.
func ProjectName(path string) string {
	var doc struct {
		Project struct {
			Name string `toml:"name"`
		} `toml:"project"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return ""
	}
	return doc.Project.Name
}

// Home is where the picker starts when it has nowhere better to go.
func Home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return string(filepath.Separator)
}
