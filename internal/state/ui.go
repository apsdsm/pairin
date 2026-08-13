package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// uiFile holds interface choices the user makes by *using* the TUI rather than
// by editing config — which view they were last in, and so on. That makes it
// state rather than configuration, so it sits beside the instance registry
// instead of in the catalog: losing it costs a keystroke, not a setting.
const uiFile = "ui.json"

// UI is the remembered interface state.
type UI struct {
	// CellStyle is the grid's last-used cell style ("plain", "boxed", "cards").
	// Stored as a string so the file stays readable and a future style can be
	// added without renumbering anything.
	CellStyle string `json:"cell_style,omitempty"`

	// BrowseDir is where the project picker last was. Projects tend to live
	// near each other, so the next one you add is usually a keystroke or two
	// from the last.
	BrowseDir string `json:"browse_dir,omitempty"`
}

// UIPath returns the file's location.
func UIPath() (string, error) {
	dir, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, uiFile), nil
}

// LoadUI reads the remembered interface state. Anything unreadable yields the
// zero value: a corrupt or missing preferences file must never stop a TUI from
// opening.
func LoadUI() UI {
	path, err := UIPath()
	if err != nil {
		return UI{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return UI{}
	}
	var ui UI
	if err := json.Unmarshal(data, &ui); err != nil {
		return UI{}
	}
	return ui
}

// SaveUI writes the remembered interface state atomically.
func SaveUI(ui UI) error {
	path, err := UIPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ui, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
