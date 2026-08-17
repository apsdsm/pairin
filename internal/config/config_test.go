package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidate_NoDependencies(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "web", Cmd: "echo hi"},
			{Name: "db", Cmd: "echo hi"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidate_ValidDependency(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "db", Cmd: "echo hi", Healthcheck: "tcp://localhost:5432"},
			{Name: "web", Cmd: "echo hi", DependsOn: []string{"db"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidate_DependencyChain(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "db", Cmd: "echo hi", Healthcheck: "tcp://localhost:5432"},
			{Name: "api", Cmd: "echo hi", Healthcheck: "http://localhost:3000", DependsOn: []string{"db"}},
			{Name: "web", Cmd: "echo hi", DependsOn: []string{"api"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidate_NonexistentDependency(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "web", Cmd: "echo hi", DependsOn: []string{"missing"}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for nonexistent dependency")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected 'does not exist' error, got: %v", err)
	}
}

func TestValidate_DependencyMissingHealthcheck(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "db", Cmd: "echo hi"},
			{Name: "web", Cmd: "echo hi", DependsOn: []string{"db"}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for dependency without healthcheck")
	}
	if !strings.Contains(err.Error(), "no healthcheck") {
		t.Fatalf("expected 'no healthcheck' error, got: %v", err)
	}
}

func TestValidate_CircularDependency_Direct(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "a", Cmd: "echo hi", Healthcheck: "tcp://localhost:1", DependsOn: []string{"b"}},
			{Name: "b", Cmd: "echo hi", Healthcheck: "tcp://localhost:2", DependsOn: []string{"a"}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for circular dependency")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Fatalf("expected 'circular dependency' error, got: %v", err)
	}
}

func TestValidate_CircularDependency_Indirect(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "a", Cmd: "echo hi", Healthcheck: "tcp://localhost:1", DependsOn: []string{"c"}},
			{Name: "b", Cmd: "echo hi", Healthcheck: "tcp://localhost:2", DependsOn: []string{"a"}},
			{Name: "c", Cmd: "echo hi", Healthcheck: "tcp://localhost:3", DependsOn: []string{"b"}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for circular dependency")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Fatalf("expected 'circular dependency' error, got: %v", err)
	}
}

func TestValidate_SelfDependency(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "a", Cmd: "echo hi", Healthcheck: "tcp://localhost:1", DependsOn: []string{"a"}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for self-dependency")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Fatalf("expected 'circular dependency' error, got: %v", err)
	}
}

func TestValidate_MultipleDependencies(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "db", Cmd: "echo hi", Healthcheck: "tcp://localhost:5432"},
			{Name: "cache", Cmd: "echo hi", Healthcheck: "tcp://localhost:6379"},
			{Name: "web", Cmd: "echo hi", DependsOn: []string{"db", "cache"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidate_DiamondDependency(t *testing.T) {
	// a -> b, a -> c, b -> d, c -> d  (diamond, not circular)
	cfg := &Config{
		Services: []Service{
			{Name: "d", Cmd: "echo hi", Healthcheck: "tcp://localhost:1"},
			{Name: "b", Cmd: "echo hi", Healthcheck: "tcp://localhost:2", DependsOn: []string{"d"}},
			{Name: "c", Cmd: "echo hi", Healthcheck: "tcp://localhost:3", DependsOn: []string{"d"}},
			{Name: "a", Cmd: "echo hi", DependsOn: []string{"b", "c"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error for diamond dependency, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Restart policy validation
// ---------------------------------------------------------------------------

func TestValidate_ValidRestartPolicies(t *testing.T) {
	for _, policy := range []string{"", "no", "always", "on-failure", "on-success"} {
		cfg := &Config{
			Services: []Service{
				{Name: "web", Cmd: "echo hi", Restart: policy},
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("expected no error for restart=%q, got: %v", policy, err)
		}
	}
}

func TestValidate_InvalidRestartPolicy(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "web", Cmd: "echo hi", Restart: "bogus"},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid restart policy")
	}
	if !strings.Contains(err.Error(), "invalid restart policy") {
		t.Fatalf("expected 'invalid restart policy' error, got: %v", err)
	}
}

func TestValidate_InvalidRestartDelay(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "web", Cmd: "echo hi", RestartDelay: "notaduration"},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid restart_delay")
	}
	if !strings.Contains(err.Error(), "invalid restart_delay") {
		t.Fatalf("expected 'invalid restart_delay' error, got: %v", err)
	}
}

func TestValidate_ValidRestartDelay(t *testing.T) {
	cfg := &Config{
		Services: []Service{
			{Name: "web", Cmd: "echo hi", RestartDelay: "5s"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ParsedRestartDelay / RestartPolicy helpers
// ---------------------------------------------------------------------------

func TestParsedRestartDelay_Default(t *testing.T) {
	svc := Service{Name: "web"}
	d := svc.ParsedRestartDelay()
	if d != 3*time.Second {
		t.Errorf("expected 3s default, got %v", d)
	}
}

func TestParsedRestartDelay_Custom(t *testing.T) {
	svc := Service{Name: "web", RestartDelay: "10s"}
	d := svc.ParsedRestartDelay()
	if d != 10*time.Second {
		t.Errorf("expected 10s, got %v", d)
	}
}

func TestParsedRestartDelay_Invalid(t *testing.T) {
	svc := Service{Name: "web", RestartDelay: "bad"}
	d := svc.ParsedRestartDelay()
	if d != 3*time.Second {
		t.Errorf("expected 3s fallback for invalid delay, got %v", d)
	}
}

func TestRestartPolicy_Default(t *testing.T) {
	svc := Service{Name: "web"}
	if p := svc.RestartPolicy(); p != "no" {
		t.Errorf("expected 'no' default, got %q", p)
	}
}

func TestRestartPolicy_Set(t *testing.T) {
	svc := Service{Name: "web", Restart: "on-failure"}
	if p := svc.RestartPolicy(); p != "on-failure" {
		t.Errorf("expected 'on-failure', got %q", p)
	}
}

// An out-of-range port is dropped and warned about. It is never an error:
// Validate failing takes the whole project down with it.
func TestValidateExposedPorts(t *testing.T) {
	for _, tt := range []struct {
		name        string
		ports       ExposeList
		wantDropped bool
	}{
		{"valid", ExposeList{{Port: 1}, {Label: "db", Port: 5432}, {Port: 65535}}, false},
		{"none", nil, false},
		{"zero", ExposeList{{Port: 0}}, true},
		{"negative", ExposeList{{Port: -1}}, true},
		{"too high", ExposeList{{Port: 65536}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Services: []Service{{Name: "svc", Cmd: "true", Exposes: tt.ports}}}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() failed over ports %v: %v", tt.ports, err)
			}

			kept := len(cfg.Services[0].Exposes)
			if tt.wantDropped {
				if kept != 0 {
					t.Errorf("kept %d of %v, want none", kept, tt.ports)
				}
				if len(cfg.Warnings) != 1 {
					t.Errorf("got %d warnings for %v, want 1", len(cfg.Warnings), tt.ports)
				}
				return
			}
			if kept != len(tt.ports) {
				t.Errorf("kept %d of %v, want all %d", kept, tt.ports, len(tt.ports))
			}
			if len(cfg.Warnings) != 0 {
				t.Errorf("warned about valid ports %v: %+v", tt.ports, cfg.Warnings)
			}
		})
	}
}

// A port list is written in whatever shape reads best at the time, and the
// bare form has to keep working because it shipped first.
func TestExposeListAcceptsEveryForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".pairinrc.toml")
	body := `[project]
name = "t"
[[services]]
name = "a"
cmd = "true"
exposes = [5432, "db:6379", ["redis", 2345], {label = "ses", port = 4500}, "9000", "  spaced : 7000 "]
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	want := ExposeList{
		{Port: 5432},
		{Label: "db", Port: 6379},
		{Label: "redis", Port: 2345},
		{Label: "ses", Port: 4500},
		{Port: 9000},
		{Label: "spaced", Port: 7000},
	}
	got := cfg.Services[0].Exposes
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Nonsense is dropped rather than kept, but the config still loads and the
// service still starts.
func TestExposeListDropsNonsense(t *testing.T) {
	for _, body := range []string{
		`exposes = ["not-a-port"]`,
		`exposes = ["db:"]`,
		`exposes = [["only-a-label"]]`,
		`exposes = [true]`,
		`exposes = "5432"`,
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, ".pairinrc.toml")
		full := "[project]\nname = \"t\"\n[[services]]\nname = \"a\"\ncmd = \"true\"\n" + body + "\n"
		if err := os.WriteFile(path, []byte(full), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		cfg, err := LoadFrom(path)
		if err != nil {
			t.Errorf("%s stopped the config loading: %v", body, err)
			continue
		}
		if len(cfg.Services[0].Exposes) != 0 {
			t.Errorf("%s kept %+v", body, cfg.Services[0].Exposes)
		}
		if len(cfg.Warnings) == 0 {
			t.Errorf("%s was dropped silently", body)
		}
	}
}

// Ports are decoration on a dashboard. A typo in one must never stop a project
// from starting, which is what happens when a config fails to load.
func TestBadExposesWarnsRatherThanFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".pairinrc.toml")
	body := `[project]
name = "t"
[[services]]
name = "a"
cmd = "true"
exposes = ["minio", "1:2:3", 70000, true, "good:9100"]
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("a bad exposes entry stopped the config loading: %v", err)
	}

	// The one good entry survives; the rest are dropped.
	got := cfg.Services[0].Exposes
	if len(got) != 1 || got[0].Label != "good" || got[0].Port != 9100 {
		t.Errorf("kept entries = %+v, want just the good one", got)
	}
	if len(cfg.Warnings) != 4 {
		t.Errorf("got %d warnings, want 4: %+v", len(cfg.Warnings), cfg.Warnings)
	}
	for _, w := range cfg.Warnings {
		if w.Service != "a" {
			t.Errorf("warning not attributed to the service: %+v", w)
		}
	}
}

// Even a completely wrong shape is survivable.
func TestExposesOfWrongTypeWarns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".pairinrc.toml")
	body := "[project]\nname = \"t\"\n[[services]]\nname = \"a\"\ncmd = \"true\"\nexposes = \"5432\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("exposes of the wrong type stopped the config loading: %v", err)
	}
	if len(cfg.Warnings) == 0 {
		t.Error("no warning for an exposes that isn't a list")
	}
}

// The label and the port are written in whatever order and with whatever
// separator came to hand. Only genuine ambiguity is refused.
func TestExposedStringIsForgiving(t *testing.T) {
	for _, tt := range []struct {
		in    string
		label string
		port  int
	}{
		{"5432", "", 5432},
		{"db:5432", "db", 5432},
		{":40111 minio", "minio", 40111},
		{"9000 minio", "minio", 9000},
		{"minio=9000", "minio", 9000},
		{"40111:minio", "minio", 40111},
		{"  spaced : 7000 ", "spaced", 7000},
		{"postgres/5432", "postgres", 5432},
	} {
		got, err := parseExposedString(tt.in)
		if err != nil {
			t.Errorf("%q: %v", tt.in, err)
			continue
		}
		if got.Label != tt.label || got.Port != tt.port {
			t.Errorf("%q = label %q port %d, want %q and %d", tt.in, got.Label, got.Port, tt.label, tt.port)
		}
	}

	for _, in := range []string{"minio", "1:2:3", "", "no numbers here"} {
		if _, err := parseExposedString(in); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
}
