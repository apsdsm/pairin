package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/apsdsm/pairin/internal/catalog"
	"github.com/apsdsm/pairin/internal/config"
	"github.com/apsdsm/pairin/internal/state"
	"github.com/spf13/cobra"
)

var (
	registerName  string
	registerGroup string
)

var registerCmd = &cobra.Command{
	Use:   "register [path]",
	Short: "Register a project so it can be started from anywhere",
	Long: "Register a .pairinrc.toml in the project catalog. With no path, registers the config\n" +
		"found from the current directory. Registered projects can be started by name from\n" +
		"anywhere: `pairin up <name>`.",
	Args: cobra.MaximumNArgs(1),
	RunE: runRegister,
}

var unregisterCmd = &cobra.Command{
	Use:   "unregister <name|path>",
	Short: "Remove a project from the catalog",
	Long:  "Remove a project from the catalog. Services and config files are left untouched.",
	Args:  cobra.ExactArgs(1),
	RunE:  runUnregister,
}

var projectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "List registered projects",
	RunE:  runProjects,
}

func init() {
	registerCmd.Flags().StringVar(&registerName, "name", "", "command-line handle for the project (default: derived from the project name)")
	registerCmd.Flags().StringVar(&registerGroup, "group", "", "optional label for grouping the listing")
	rootCmd.AddCommand(registerCmd)
	rootCmd.AddCommand(unregisterCmd)
	rootCmd.AddCommand(projectsCmd)
}

func runRegister(cmd *cobra.Command, args []string) error {
	path := ""
	if len(args) == 1 {
		path = args[0]
	}

	cfg, err := loadConfigAt(path)
	if err != nil {
		return err
	}

	cat, err := catalog.Load()
	if err != nil {
		return fmt.Errorf("loading catalog: %w", err)
	}

	if existing, ok := cat.ByConfig(cfg.Path); ok {
		fmt.Printf("Already registered as %q.\n", existing.Name)
		return nil
	}

	// Registering is a deliberate act, so the entry is pinned: it stays in the
	// dashboard whether or not the project is running.
	entry := catalog.Project{
		Name:    registerName,
		Display: cfg.Project.Name,
		Config:  cfg.Path,
		Group:   registerGroup,
	}
	if _, err := cat.Add(entry); err != nil {
		return err
	}
	if err := cat.Save(); err != nil {
		return fmt.Errorf("saving catalog: %w", err)
	}

	added, _ := cat.ByConfig(cfg.Path)
	fmt.Printf("Registered %q -> %s\n", added.Name, added.Config)
	fmt.Printf("Start it from anywhere with: pairin up %s\n", added.Name)
	return nil
}

func runUnregister(cmd *cobra.Command, args []string) error {
	cat, err := catalog.Load()
	if err != nil {
		return fmt.Errorf("loading catalog: %w", err)
	}
	removed, err := cat.Remove(args[0])
	if err != nil {
		return err
	}
	if err := cat.Save(); err != nil {
		return fmt.Errorf("saving catalog: %w", err)
	}
	fmt.Printf("Unregistered %q (%s)\n", removed.Name, removed.Config)
	return nil
}

func runProjects(cmd *cobra.Command, args []string) error {
	cat, err := catalog.Load()
	if err != nil {
		return fmt.Errorf("loading catalog: %w", err)
	}
	if len(cat.Projects) == 0 {
		fmt.Println("No projects registered.")
		fmt.Println("Register one with `pairin register` from a project directory.")
		return nil
	}

	// Cross-reference the running supervisors so the listing says which of
	// these are actually up right now.
	running := map[string]int{}
	if insts, err := state.ListInstances(); err == nil {
		for _, inst := range insts {
			running[inst.ConfigPath] = inst.SupervisorPID
		}
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tGROUP\tPINNED\tSTATUS\tCONFIG")
	for _, p := range cat.Projects {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.Name,
			dashIfEmpty(p.Group),
			pinnedLabel(p),
			projectStatus(p, running),
			p.Config,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("Unpinned projects were added automatically by `pairin up`; they drop out of")
	fmt.Println("`pairin dash` once they stop. Press p in the dashboard to pin or unpin one.")
	return nil
}

// projectStatus describes a catalog entry's current state. A config that has
// been moved or deleted is called out rather than quietly reported as stopped —
// that's a broken entry, not an idle one.
func projectStatus(p catalog.Project, running map[string]int) string {
	if pid, ok := running[p.Config]; ok {
		return fmt.Sprintf("running (pid %d)", pid)
	}
	if _, err := os.Stat(p.Config); err != nil {
		return "MISSING config"
	}
	return "stopped"
}

func pinnedLabel(p catalog.Project) string {
	if p.Pinned() {
		return "yes"
	}
	return "auto"
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// loadConfigAt loads a config from an explicit path (file or directory), or
// searches from the cwd when path is empty.
func loadConfigAt(path string) (*config.Config, error) {
	if path == "" {
		cfg, err := config.Load()
		if err != nil {
			return nil, fmt.Errorf("loading config: %w", err)
		}
		return cfg, nil
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving path: %w", err)
	}
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		abs = filepath.Join(abs, ".pairinrc.toml")
	}
	cfg, err := config.LoadFrom(abs)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	return cfg, nil
}
