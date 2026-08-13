package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSlugify(t *testing.T) {
	tests := []struct{ in, want string }{
		{"JJC2 (localdev)", "jjc2-localdev"},
		{"Kintai CREW (localdev)", "kintai-crew-localdev"},
		{"acme-api", "acme-api"},
		{"  spaced  out  ", "spaced-out"},
		{"under_scores", "under-scores"},
		{"!!!", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := Slugify(tt.in); got != tt.want {
			t.Errorf("Slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAddDerivesUniqueNames(t *testing.T) {
	c := &Catalog{}

	if _, err := c.Add(Project{Display: "LGC (localdev)", Config: "/home/n/lgc_main/.pairinrc.toml"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := c.Projects[0].Name; got != "lgc-localdev" {
		t.Errorf("derived name = %q, want %q", got, "lgc-localdev")
	}

	// Same display name, different checkout: qualify with the directory rather
	// than silently colliding.
	if _, err := c.Add(Project{Display: "LGC (localdev)", Config: "/home/n/lgc_fork/.pairinrc.toml"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := c.Projects[1].Name; got != "lgc-localdev-lgc-fork" {
		t.Errorf("qualified name = %q, want %q", got, "lgc-localdev-lgc-fork")
	}
}

func TestAddIsIdempotentAndPreservesCustomNames(t *testing.T) {
	c := &Catalog{}
	if _, err := c.Add(Project{Name: "mine", Display: "Some Project", Config: "/p/.pairinrc.toml"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	added, err := c.Add(Project{Display: "Some Project", Config: "/p/.pairinrc.toml"})
	if err != nil {
		t.Fatalf("re-Add: %v", err)
	}
	if added {
		t.Error("re-adding the same config path reported a new entry")
	}
	if len(c.Projects) != 1 {
		t.Fatalf("catalog has %d entries, want 1", len(c.Projects))
	}
	if c.Projects[0].Name != "mine" {
		t.Errorf("re-adding overwrote the chosen name: %q", c.Projects[0].Name)
	}
}

func TestAddRejectsDuplicateExplicitName(t *testing.T) {
	c := &Catalog{}
	if _, err := c.Add(Project{Name: "api", Config: "/a/.pairinrc.toml"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := c.Add(Project{Name: "api", Config: "/b/.pairinrc.toml"}); err == nil {
		t.Error("adding a duplicate name was allowed")
	}
}

func TestFind(t *testing.T) {
	c := &Catalog{Projects: []Project{
		{Name: "acme-api", Config: "/home/n/acme-api/.pairinrc.toml"},
		{Name: "acme-web", Config: "/home/n/acme-web/.pairinrc.toml"},
		{Name: "storefront", Config: "/home/n/storefront/.pairinrc.toml"},
	}}

	t.Run("exact name", func(t *testing.T) {
		p, err := c.Find("acme-api")
		if err != nil || p.Name != "acme-api" {
			t.Errorf("Find(acme-api) = %v, %v", p.Name, err)
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		if _, err := c.Find("ACME-API"); err != nil {
			t.Errorf("Find(ACME-API): %v", err)
		}
	})

	t.Run("config path", func(t *testing.T) {
		p, err := c.Find("/home/n/storefront/.pairinrc.toml")
		if err != nil || p.Name != "storefront" {
			t.Errorf("Find by path = %v, %v", p.Name, err)
		}
	})

	t.Run("unique prefix", func(t *testing.T) {
		p, err := c.Find("store")
		if err != nil || p.Name != "storefront" {
			t.Errorf("Find(store) = %v, %v", p.Name, err)
		}
	})

	// An ambiguous prefix must refuse rather than pick one — starting the wrong
	// project's services is not a recoverable mistake.
	t.Run("ambiguous prefix refuses", func(t *testing.T) {
		_, err := c.Find("acme")
		var amb *ErrAmbiguous
		if !errors.As(err, &amb) {
			t.Fatalf("Find(acme) error = %v, want ErrAmbiguous", err)
		}
		if len(amb.Matches) != 2 {
			t.Errorf("ambiguous match list = %v, want 2 entries", amb.Matches)
		}
	})

	t.Run("no match", func(t *testing.T) {
		_, err := c.Find("nope")
		var nf *ErrNotFound
		if !errors.As(err, &nf) {
			t.Errorf("Find(nope) error = %v, want ErrNotFound", err)
		}
	})
}

func TestRemove(t *testing.T) {
	c := &Catalog{Projects: []Project{
		{Name: "a", Config: "/a/.pairinrc.toml"},
		{Name: "b", Config: "/b/.pairinrc.toml"},
	}}

	if _, err := c.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(c.Projects) != 1 || c.Projects[0].Name != "b" {
		t.Errorf("after removal: %+v", c.Projects)
	}
	if _, err := c.Remove("a"); err == nil {
		t.Error("removing a missing project was allowed")
	}
}

func TestLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// A missing catalog is an empty one, not an error.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if len(c.Projects) != 0 {
		t.Fatalf("fresh catalog has %d projects", len(c.Projects))
	}

	if _, err := c.Add(Project{Display: "Acme API", Config: filepath.Join(dir, "acme", ".pairinrc.toml"), Group: "work"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(reloaded.Projects) != 1 {
		t.Fatalf("reloaded %d projects, want 1", len(reloaded.Projects))
	}
	got := reloaded.Projects[0]
	if got.Name != "acme-api" || got.Group != "work" || got.Display != "Acme API" {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestSaveGoesUnderConfigHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join(dir, "pairin", "projects.toml"); path != want {
		t.Errorf("Path = %q, want %q", path, want)
	}
}

// Entries written before pinning existed have no `auto` field, and must keep
// showing: a user who ran `pairin register` did so deliberately.
func TestPinnedDefaultsToTrueForExistingEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if err := os.MkdirAll(filepath.Join(dir, "pairin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := "[[project]]\n  name = \"legacy\"\n  config = \"/p/.pairinrc.toml\"\n"
	if err := os.WriteFile(filepath.Join(dir, "pairin", "projects.toml"), []byte(old), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Projects[0].Pinned() {
		t.Error("an entry from before pinning existed came back unpinned")
	}
}

func TestSetPinned(t *testing.T) {
	c := &Catalog{}
	if _, err := c.Add(Project{Name: "temp", Config: "/tmp/x/.pairinrc.toml", Auto: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if c.Projects[0].Pinned() {
		t.Fatal("an auto-added entry started out pinned")
	}

	if _, err := c.SetPinned("/tmp/x/.pairinrc.toml", "X", true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	if !c.Projects[0].Pinned() {
		t.Error("pinning did not take")
	}

	if _, err := c.SetPinned("/tmp/x/.pairinrc.toml", "X", false); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	if c.Projects[0].Pinned() {
		t.Error("unpinning did not take")
	}

	// A project started by path has no entry until someone asks to keep it.
	if _, err := c.SetPinned("/tmp/y/.pairinrc.toml", "Y", true); err != nil {
		t.Fatalf("SetPinned on an unknown project: %v", err)
	}
	entry, ok := c.ByConfig("/tmp/y/.pairinrc.toml")
	if !ok {
		t.Fatal("pinning an unregistered project did not add it")
	}
	if !entry.Pinned() {
		t.Error("newly pinned project came back unpinned")
	}
}
