// Package catalog is the user's list of known pairin projects — the thing that
// lets `pairin up acme-api` work from anywhere instead of requiring a cd into
// the right directory first.
//
// It lives in the config directory rather than the state directory
// ($XDG_CONFIG_HOME/pairin/projects.toml, default ~/.config/pairin/), because
// it is curated rather than derived: it survives a state cleanup, it can be
// hand-edited, and it can reasonably be checked into a dotfiles repo. The
// separate registry under $XDG_STATE_HOME records which supervisors happen to
// be *running* right now, which is a different question entirely.
package catalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
)

// Project is one registered .pairinrc.toml.
type Project struct {
	// Name is the command-line handle — a slug, unique within the catalog.
	// It is not the project's display name, which may contain spaces and
	// parentheses and make a poor thing to type.
	Name string `toml:"name"`

	// Display is the [project].name from the config, kept so listings can show
	// something friendlier than the slug.
	Display string `toml:"display,omitempty"`

	// Config is the absolute path of the .pairinrc.toml.
	Config string `toml:"config"`

	// Group is a free-form label for organizing the listing.
	Group string `toml:"group,omitempty"`
}

// Catalog is the whole file.
type Catalog struct {
	Projects []Project `toml:"project"`
}

// ErrAmbiguous is returned when a lookup matches more than one project.
type ErrAmbiguous struct {
	Query   string
	Matches []string
}

func (e *ErrAmbiguous) Error() string {
	return fmt.Sprintf("%q matches %d projects (%s) — be more specific",
		e.Query, len(e.Matches), strings.Join(e.Matches, ", "))
}

// ErrNotFound is returned when a lookup matches nothing.
type ErrNotFound struct{ Query string }

func (e *ErrNotFound) Error() string {
	return fmt.Sprintf("no registered project matching %q (run `pairin projects` to see the list)", e.Query)
}

// Dir returns the directory holding the catalog file.
func Dir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home dir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "pairin"), nil
}

// Path returns the catalog file's path.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "projects.toml"), nil
}

// Load reads the catalog. A missing file is an empty catalog, not an error —
// the common case is a user who has never registered anything.
func Load() (*Catalog, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Catalog{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Catalog
	if err := toml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the catalog atomically.
func (c *Catalog) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	c.sort()

	var b strings.Builder
	b.WriteString("# pairin project catalog\n")
	b.WriteString("# Registered projects, so `pairin up <name>` works from anywhere.\n")
	b.WriteString("# Managed by `pairin register` / `pairin unregister`; safe to hand-edit.\n\n")
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return fmt.Errorf("encoding catalog: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Catalog) sort() {
	sort.Slice(c.Projects, func(i, j int) bool {
		if c.Projects[i].Group != c.Projects[j].Group {
			return c.Projects[i].Group < c.Projects[j].Group
		}
		return c.Projects[i].Name < c.Projects[j].Name
	})
}

// ByConfig returns the entry for an absolute config path.
func (c *Catalog) ByConfig(configPath string) (Project, bool) {
	abs := absOrSelf(configPath)
	for _, p := range c.Projects {
		if p.Config == abs {
			return p, true
		}
	}
	return Project{}, false
}

// Add registers a project, returning false if that config path was already
// present. An existing entry is left untouched: re-running `pairin up` in a
// project must not clobber a name the user chose deliberately.
func (c *Catalog) Add(p Project) (bool, error) {
	p.Config = absOrSelf(p.Config)
	if _, exists := c.ByConfig(p.Config); exists {
		return false, nil
	}

	if p.Name == "" {
		p.Name = c.uniqueName(p.Display, p.Config)
	} else if c.hasName(p.Name) {
		return false, fmt.Errorf("a project named %q is already registered", p.Name)
	}

	c.Projects = append(c.Projects, p)
	return true, nil
}

// Remove deletes the entry matching query (by name or config path).
func (c *Catalog) Remove(query string) (Project, error) {
	p, err := c.Find(query)
	if err != nil {
		return Project{}, err
	}
	for i, existing := range c.Projects {
		if existing.Config == p.Config {
			c.Projects = append(c.Projects[:i], c.Projects[i+1:]...)
			return p, nil
		}
	}
	return Project{}, &ErrNotFound{Query: query}
}

// Find resolves a query to a project. It tries, in order: an exact name match,
// an exact config path, and finally a unique prefix of a name — typing
// `pairin up acme` should work when there's only one project starting that way,
// but must refuse rather than guess when there are two.
func (c *Catalog) Find(query string) (Project, error) {
	if query == "" {
		return Project{}, &ErrNotFound{Query: query}
	}

	lower := strings.ToLower(query)
	for _, p := range c.Projects {
		if strings.ToLower(p.Name) == lower {
			return p, nil
		}
	}

	abs := absOrSelf(query)
	for _, p := range c.Projects {
		if p.Config == abs {
			return p, nil
		}
	}

	var matches []Project
	for _, p := range c.Projects {
		if strings.HasPrefix(strings.ToLower(p.Name), lower) {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 0:
		return Project{}, &ErrNotFound{Query: query}
	case 1:
		return matches[0], nil
	default:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.Name
		}
		return Project{}, &ErrAmbiguous{Query: query, Matches: names}
	}
}

func (c *Catalog) hasName(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range c.Projects {
		if strings.ToLower(p.Name) == lower {
			return true
		}
	}
	return false
}

// uniqueName derives a command-line handle. It prefers the display name, falls
// back to the config's directory, and on a collision qualifies with the parent
// directory before finally counting up — several projects legitimately share a
// display name across checkouts.
func (c *Catalog) uniqueName(display, configPath string) string {
	base := Slugify(display)
	if base == "" {
		base = Slugify(filepath.Base(filepath.Dir(configPath)))
	}
	if base == "" {
		base = "project"
	}
	if !c.hasName(base) {
		return base
	}

	if qualifier := Slugify(filepath.Base(filepath.Dir(configPath))); qualifier != "" && qualifier != base {
		candidate := base + "-" + qualifier
		if !c.hasName(candidate) {
			return candidate
		}
	}

	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !c.hasName(candidate) {
			return candidate
		}
	}
}

// Slugify turns a display name into something typeable: lowercase, with runs of
// non-alphanumerics collapsed to single dashes. "JJC2 (localdev)" becomes
// "jjc2-localdev".
func Slugify(s string) string {
	var b strings.Builder
	lastDash := true // suppresses a leading dash
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func absOrSelf(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
