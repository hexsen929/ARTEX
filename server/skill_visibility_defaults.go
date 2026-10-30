package server

import (
	"fmt"
	"log"
	"path/filepath"
	"sort"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/skill"
)

// seedInstalledBuiltinSkillVisibility grants only defaults that can actually be
// loaded from THIS installation. This is metadata inspection: it does not invoke
// Skill instructions, connect MCP servers, or install optional/private Skills.
func seedInstalledBuiltinSkillVisibility(pg *db.DB, root string) error {
	if pg == nil || pg.DB == nil {
		return fmt.Errorf("Skill 默认授权数据库未就绪")
	}
	available, skipped, err := installedBuiltinSkillDefaults(root)
	if err != nil {
		// Keep uninitialized roles unmarked. Their runtime assembly reports the
		// missing initialization explicitly instead of running on unknown defaults.
		return err
	}
	if err := pg.SeedBuiltinSkillVisibility(available); err != nil {
		return err
	}
	if len(skipped) > 0 {
		// Only names from the public default policy are logged. Never log parser
		// text, absolute paths, instructions, dependency configuration or secrets.
		log.Printf("[skills] %d 个默认 Skill 未安装或不可加载，未新增其授权；既有选择保持不变: %v", len(skipped), skipped)
	}
	return nil
}

func installedBuiltinSkillDefaults(root string) (available, skipped []string, err error) {
	if root == "" {
		return nil, nil, fmt.Errorf("Skill 默认目录未配置，默认授权未初始化")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, nil, fmt.Errorf("Skill 默认目录无效，默认授权未初始化")
	}
	catalog, err := inspectSkillCatalog(root)
	if err != nil {
		return nil, nil, fmt.Errorf("Skill 默认目录不可读取，默认授权未初始化")
	}
	registry, err := skill.LoadDir(root)
	if err != nil || registry == nil {
		return nil, nil, fmt.Errorf("Skill 默认目录未进入加载器，默认授权未初始化")
	}
	// Match the SDK's final name-indexed registry too. A top-level Markdown
	// alias can overwrite a directory Skill even when its own index is valid.
	loaded := map[string]string{}
	for _, entry := range registry.List() {
		loaded[entry.Name] = filepath.Clean(entry.Dir)
	}
	available = []string{}
	skipped = []string{}
	for _, name := range db.BuiltinSkillDefaultNames() {
		inspection, exists := catalog[name]
		if !exists || !inspection.Loadable || loaded[inspection.DeclaredName] != filepath.Join(root, name) {
			skipped = append(skipped, name)
			continue
		}
		available = append(available, name)
	}
	sort.Strings(available)
	sort.Strings(skipped)
	return available, skipped, nil
}
