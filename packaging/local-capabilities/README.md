# 本地技能与工具配置

本目录保存可迁移的能力定义，不包含运行数据库、目标数据、凭据、日志、二进制或 Python 虚拟环境。技能正文及附属脚本位于仓库的 `skills/`。

## 内容

- `profile.json`：19 个自定义工具定义、74 条按智能体 key 标识的技能绑定，以及清理技能的提示词追加入口。
- `mcp-profile.json`：5 个 MCP 服务定义和 27 条按服务名/智能体 key 标识的绑定；环境中的凭据及本机路径使用占位符。
- `browser-config-templates.json`：两个浏览器 MCP 的本地配置模板，代理凭据、证书指纹和浏览器路径须在目标机器填写。`jshook-guard.mjs` 保存现用的 stdio/RSS 生命周期保护脚本。
- `installed-inventory.json`：40 项已安装工具资源的版本、平台和摘要参考。包含主机工具及仅供目标侧使用的资源；它不是安装清单，不应直接拿来注册可执行工具。
- `../tools-manifest.json`、`../python-tools-sources.json` 及对应的 prepare/register 脚本：可重复安装的固定来源。二进制与安装包使用项目 Release，不放入 Git 源码树。

`skills/chains-*/SKILL.md` 保留仓库中已修复的 `${SKILL_DIR}` 路径写法；与旧本机副本相比，参考正文未变。`zz-probe` 是保留的本地探针技能，不增加默认角色绑定。

## 在另一套 ARTEX 上恢复

1. 按 [MACOS-TOOLS.md](../MACOS-TOOLS.md) 安装适用平台的工具，并用 `register-tools.py --plan` 核对；确认后用 `--apply` 注册缺失的主机工具。目标侧二进制不要注册为本机可运行工具。
2. 将需要的技能目录放入目标安装的技能根目录。在智能体管理中参考 `profile.json` 的 `skill_bindings` 设置技能权限。按 `agent_key` 匹配，不复制数据库数字 ID；不存在的自定义角色留待人工映射。已有权限取舍应保留，不整表覆盖。
3. `profile.json` 中的 `tools` 使用 ARTEX 自定义工具的字段结构。在工具管理页创建缺失条目；已有同名工具先比较配置，不直接覆盖。shell 类型是 Bash 环境说明，依赖第 1 步已安装的程序。
4. 将 `${ARTEX_DATA_DIR}`、`${ARTEX_SKILL_DIR}` 替换为目标实例的实际路径。Shodan HTTP 工具里的 `${SHODAN_API_KEY}` 必须在本机导入时换成本机密钥；ARTEX 的 HTTP 配置不会自动展开这个占位符。未配置密钥时不要启用这些 HTTP 条目。不要把替换后的文件提交到 Git。
5. Shodan 技能脚本直接读取运行环境的 `SHODAN_API_KEY`，不包含内置密钥。须由启动 ARTEX 的进程环境提供；缺少时脚本报错且不发请求。
6. 清理技能默认策略为 `preview`。按 [接入说明](../../skills/artex-intent-cleanup/references/activation.md) 追加提示词，不覆盖现有提示词。复制技能或导入绑定不会开启实际删除。

MCP 在管理页参考 `mcp-profile.json` 恢复：先准备对应运行环境，再展开路径和凭据占位符，最后核对角色绑定。jshook 使用 `@jshookmcp/jshook` 0.4.2 的独立安装目录；Node 与浏览器二进制路径以目标机器为准。浏览器模板的占位符也需要显式替换，不能直接当配置运行；模板中记录的是原有行为，不代表它们适合任意新部署。当前实例的实际代理密钥、证书、配置与运行依赖留在本机。

这里没有导入器，也不会自动修改正在运行的 ARTEX。保留 profile 中的占位符和默认预览策略，才能安全地继续同步源码。
