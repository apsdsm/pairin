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

func TestValidateExposedPorts(t *testing.T) {
	for _, tt := range []struct {
		name    string
		ports   ExposeList
		wantErr bool
	}{
		{"valid", ExposeList{{Port: 1}, {Label: "db", Port: 5432}, {Port: 65535}}, false},
		{"none", nil, false},
		{"zero", ExposeList{{Port: 0}}, true},
		{"negative", ExposeList{{Port: -1}}, true},
		{"too high", ExposeList{{Port: 65536}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Services: []Service{{Name: "svc", Cmd: "true", Exposes: tt.ports}}}
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("Validate() accepted ports %v", tt.ports)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate() rejected ports %v: %v", tt.ports, err)
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

func TestExposeListRejectsNonsense(t *testing.T) {
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
		if _, err := LoadFrom(path); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}
