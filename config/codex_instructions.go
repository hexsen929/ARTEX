package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

const instructionsTextLimit = 1024 * 1024

// CodexInstructions contains only instruction-related values from one Codex
// config file. It does not load credentials, providers, or project instructions.
type CodexInstructions struct {
	ConfigPath            string
	ModelInstructionsPath string
	BaseInstructions      string
	DeveloperInstructions string
}

// LoadCodexInstructions reads the two top-level Codex instruction settings.
// An empty configPath selects CODEX_HOME/config.toml, or ~/.codex/config.toml
// when CODEX_HOME is not set. A relative model_instructions_file is anchored
// to the selected TOML file's directory, not the process working directory.
// Missing files and invalid configuration are errors, never silent fallbacks.
func LoadCodexInstructions(configPath string) (CodexInstructions, error) {
	var result CodexInstructions
	if strings.TrimSpace(configPath) == "" {
		codexDir := strings.TrimSpace(os.Getenv("CODEX_HOME"))
		if codexDir == "" {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return result, fmt.Errorf("resolve Codex configuration directory: %w", err)
			}
			codexDir = filepath.Join(homeDir, ".codex")
		}
		configPath = filepath.Join(codexDir, "config.toml")
	}
	path, err := resolveInstructionsPath(configPath, "")
	if err != nil {
		return result, fmt.Errorf("resolve Codex configuration path: %w", err)
	}
	result.ConfigPath = path
	raw, err := readInstructionsTextFile(path, "Codex configuration", true)
	if err != nil {
		return result, err
	}
	// A real TOML decoder handles quoted keys, escaping and multiline strings,
	// and prevents provider/profile tables from being mistaken for root keys.
	var selected struct {
		ModelInstructionsFile string `toml:"model_instructions_file"`
		DeveloperInstructions string `toml:"developer_instructions"`
	}
	if err := toml.Unmarshal([]byte(raw), &selected); err != nil {
		// Decoder messages can contain source values. Return the location only,
		// because unrelated TOML settings can contain API keys and credentials.
		var decodeErr *toml.DecodeError
		if errors.As(err, &decodeErr) {
			row, column := decodeErr.Position()
			return result, fmt.Errorf("invalid Codex configuration TOML at line %d, column %d (%s)", row, column, path)
		}
		return result, fmt.Errorf("invalid Codex configuration TOML (%s)", path)
	}
	if !utf8.ValidString(selected.DeveloperInstructions) || strings.IndexByte(selected.DeveloperInstructions, 0) >= 0 {
		return result, fmt.Errorf("Codex developer_instructions must contain UTF-8 text without NUL (%s)", path)
	}
	result.DeveloperInstructions = selected.DeveloperInstructions
	if strings.TrimSpace(selected.ModelInstructionsFile) == "" {
		return result, nil
	}
	modelPath, err := resolveInstructionsPath(selected.ModelInstructionsFile, filepath.Dir(path))
	if err != nil {
		return result, fmt.Errorf("resolve Codex model_instructions_file: %w", err)
	}
	result.ModelInstructionsPath = modelPath
	result.BaseInstructions, err = ReadInstructionsTextFile(modelPath)
	if err != nil {
		return result, err
	}
	return result, nil
}

// ReadInstructionsTextFile reads a regular, nonempty UTF-8 instruction file of
// at most 1 MiB. It removes a UTF-8 BOM but preserves every other byte, including
// whitespace and template-like text. Symlinks are rejected.
func ReadInstructionsTextFile(path string) (string, error) {
	return readInstructionsTextFile(path, "model instructions", false)
}

func readInstructionsTextFile(path, kind string, allowEmpty bool) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("read %s file: %w", kind, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s file must be a regular file: %s", kind, path)
	}
	if info.Size() > instructionsTextLimit {
		return "", fmt.Errorf("%s file exceeds 1 MiB: %s", kind, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read %s file: %w", kind, err)
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s file: %w", kind, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return "", fmt.Errorf("%s file changed while opening: %s", kind, path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, instructionsTextLimit+1))
	if err != nil {
		return "", fmt.Errorf("read %s file: %w", kind, err)
	}
	if len(raw) > instructionsTextLimit {
		return "", fmt.Errorf("%s file exceeds 1 MiB: %s", kind, path)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return "", fmt.Errorf("%s file must contain UTF-8 text without NUL: %s", kind, path)
	}
	if !allowEmpty && len(bytes.TrimSpace(raw)) == 0 {
		return "", fmt.Errorf("%s file is empty: %s", kind, path)
	}
	return string(raw), nil
}

func resolveInstructionsPath(path, baseDir string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("instruction path is empty or contains NUL")
	}
	if path == "~" || strings.HasPrefix(path, "~/") || (filepath.Separator == '\\' && strings.HasPrefix(path, `~\`)) {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = homeDir
		} else {
			path = filepath.Join(homeDir, path[2:])
		}
	}
	if !filepath.IsAbs(path) && baseDir != "" {
		path = filepath.Join(baseDir, path)
	}
	return filepath.Abs(path)
}
