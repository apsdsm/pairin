package browse

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestIsConfigName(t *testing.T) {
	yes := []string{".pairinrc.toml", ".pairinrc.localdev.toml", ".pairinrc.staging.toml"}
	no := []string{"pairinrc.toml", ".pairinrc", "config.toml", ".pairinrc.toml.bak", "README.md"}

	for _, n := range yes {
		if !IsConfigName(n) {
			t.Errorf("IsConfigName(%q) = false, want true", n)
		}
	}
	for _, n := range no {
		if IsConfigName(n) {
			t.Errorf("IsConfigName(%q) = true, want false", n)
		}
	}
}

// The listing is configs first, then directories, with the parent on top —
// what you came for above the places you'd go looking for it.
func TestReadOrdersConfigsBeforeDirectories(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")

	write(t, filepath.Join(proj, ".pairinrc.toml"), "[project]\nname = \"Main\"\n")
	write(t, filepath.Join(proj, ".pairinrc.localdev.toml"), "[project]\nname = \"Main (localdev)\"\n")
	write(t, filepath.Join(proj, "README.md"), "ignore me")
	if err := os.MkdirAll(filepath.Join(proj, "backend"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(proj, "frontend"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	entries, err := Read(proj, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	want := []string{"../", ".pairinrc.localdev.toml", ".pairinrc.toml", "backend/", "frontend/"}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, names[i], want[i])
		}
	}
}

// Picking by project name is the point: several projects keep more than one
// config side by side, and the filenames don't tell them apart.
func TestReadResolvesProjectNames(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".pairinrc.toml"), "[project]\nname = \"Jinji Crew 2\"\n")
	write(t, filepath.Join(root, ".pairinrc.localdev.toml"), "[project]\nname = \"JJC2 (localdev)\"\n")

	entries, err := Read(root, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	got := map[string]string{}
	for _, e := range entries {
		if e.IsConfig {
			got[e.Name] = e.Project
		}
	}
	if got[".pairinrc.toml"] != "Jinji Crew 2" {
		t.Errorf("project name = %q, want %q", got[".pairinrc.toml"], "Jinji Crew 2")
	}
	if got[".pairinrc.localdev.toml"] != "JJC2 (localdev)" {
		t.Errorf("project name = %q, want %q", got[".pairinrc.localdev.toml"], "JJC2 (localdev)")
	}
}

// A config that wouldn't load — an invalid service — must still be listed with
// its name, or it becomes impossible to find and fix.
func TestProjectNameSurvivesAnInvalidConfig(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".pairinrc.toml")
	write(t, path, "[project]\nname = \"Broken\"\n\n[[services]]\nname = \"x\"\ndepends_on = [\"nope\"]\n")

	if got := ProjectName(path); got != "Broken" {
		t.Errorf("ProjectName = %q, want %q", got, "Broken")
	}
}

// The count is what makes browsing bearable: it says which directories are
// worth opening before you open them.
func TestReadCountsConfigsInSubdirectories(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "two", ".pairinrc.toml"), "[project]\nname = \"A\"\n")
	write(t, filepath.Join(root, "two", ".pairinrc.localdev.toml"), "[project]\nname = \"B\"\n")
	write(t, filepath.Join(root, "one", ".pairinrc.toml"), "[project]\nname = \"C\"\n")
	if err := os.MkdirAll(filepath.Join(root, "none"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	entries, err := Read(root, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	counts := map[string]int{}
	for _, e := range entries {
		if e.IsDir && !e.IsParent {
			counts[e.Name] = e.Configs
		}
	}
	for name, want := range map[string]int{"two/": 2, "one/": 1, "none/": 0} {
		if counts[name] != want {
			t.Errorf("%s has %d configs, want %d", name, counts[name], want)
		}
	}
}

func TestReadSkipsNoiseDirectories(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"node_modules", ".git", ".pairin", "src"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	entries, err := Read(root, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, e := range entries {
		switch e.Name {
		case "node_modules/", ".git/", ".pairin/":
			t.Errorf("%q should not be listed", e.Name)
		}
	}

	found := false
	for _, e := range entries {
		if e.Name == "src/" {
			found = true
		}
	}
	if !found {
		t.Error("an ordinary directory was skipped")
	}
}

// Configs already in the catalog are flagged, and pinned ones distinguished
// from merely-catalogued ones: an unpinned project isn't visible in the
// dashboard, so the picker has to offer it rather than call it already added.
func TestReadFlagsCatalogueMembership(t *testing.T) {
	root := t.TempDir()
	pinned := filepath.Join(root, ".pairinrc.toml")
	hidden := filepath.Join(root, ".pairinrc.hidden.toml")
	fresh := filepath.Join(root, ".pairinrc.new.toml")
	write(t, pinned, "[project]\nname = \"Pinned\"\n")
	write(t, hidden, "[project]\nname = \"Hidden\"\n")
	write(t, fresh, "[project]\nname = \"New\"\n")

	entries, err := Read(root, func(p string) (bool, bool) {
		switch p {
		case pinned:
			return true, true
		case hidden:
			return true, false
		}
		return false, false
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	got := map[string][2]bool{}
	for _, e := range entries {
		if e.IsConfig {
			got[e.Path] = [2]bool{e.Added, e.Pinned}
		}
	}
	for path, want := range map[string][2]bool{
		pinned: {true, true},
		hidden: {true, false},
		fresh:  {false, false},
	} {
		if got[path] != want {
			t.Errorf("%s = added:%v pinned:%v, want added:%v pinned:%v",
				filepath.Base(path), got[path][0], got[path][1], want[0], want[1])
		}
	}
}

// The filesystem root has no parent to go up to.
func TestReadAtRootHasNoParentEntry(t *testing.T) {
	entries, err := Read(string(filepath.Separator), nil)
	if err != nil {
		t.Skipf("cannot read root: %v", err)
	}
	for _, e := range entries {
		if e.IsParent {
			t.Error("root listing offered a parent directory")
		}
	}
}

func TestReadUnreadableDirectory(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "does-not-exist"), nil); err == nil {
		t.Error("reading a missing directory succeeded")
	}
}
