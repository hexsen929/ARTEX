package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigureExecutionEnvironment supplies the same per-process tool/library
// namespace as the original launch wrapper. It never modifies a login profile,
// system PATH, routing table or unrelated home directory.
func ConfigureExecutionEnvironment(dataDir string) error {
	data, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	base, err := filepath.Abs(BaseDir())
	if err != nil {
		return err
	}
	values := map[string]string{"ARTEX_HOME": base, "DSH_HOME": base, "ARTEX_DATA_DIR": data}
	library := strings.TrimSpace(os.Getenv("ARTEX_KB_DIR"))
	if library == "" {
		library = strings.TrimSpace(Load().KnowledgeDir)
	}
	if library != "" {
		if !filepath.IsAbs(library) {
			library = filepath.Join(base, library)
		}
		values["ARTEX_KB_DIR"] = filepath.Clean(library)
	}
	for key, value := range values {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			if err = os.Setenv(key, value); err != nil {
				return fmt.Errorf("configure %s: %w", key, err)
			}
		}
	}
	tools := filepath.Join(data, "tools")
	if info, err := os.Lstat(tools); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		path := os.Getenv("PATH")
		present := false
		for _, part := range filepath.SplitList(path) {
			if filepath.Clean(part) == tools {
				present = true
				break
			}
		}
		if !present {
			if err = os.Setenv("PATH", tools+string(os.PathListSeparator)+path); err != nil {
				return err
			}
		}
	}
	return nil
}
