# Mac ARM64 外部工具部署

平台安装包包含准备脚本和来源清单，外部工具保存在实例自己的 `data/tools/`。
宿主工具、目标端文件和模板资源分别记录，不把文件存在视作功能验收完成。

## 运行端和依赖

- 本机成品：suo5、chisel、gogo、naabu、ProjectDiscovery httpx、katana、fscan、ligolo-proxy、nuclei、ffuf。
- nmap 使用 Homebrew 成品 bottle，禁止缺失 bottle 时回落为本机源码编译。
- Impacket、NetExec、pypykatz 和 Penelope 使用各自的 Python 3.11 环境。
- Chisel 和 ligolo-agent 的目标端文件按 `targets/<os>-<arch>/` 保存。现有 Chisel 自动投递和进程回收支持 Linux 目标；Windows/macOS 文件可准备，但相应自动生命周期尚未实现。
- suo5 服务端脚本、PEASS、Windows 凭据工具和 LaZagne Mac 源码属于目标资源，不自动注册为本机可执行工具。
- naabu 的部分扫描模式需要额外权限或 libpcap；ligolo 的 TUN/路由配置需要系统权限；katana 无头模式需要 Chrome/Chromium。安装脚本不自动提权或修改系统路由。

## 原生工具

在实例根目录执行，默认只列计划：

```sh
python3 packaging/prepare-tools.py --platform darwin/arm64 --destination "$PWD/data/tools" --plan
```

显式安装所有适用的固定版本工具及目标资源：

```sh
python3 packaging/prepare-tools.py --platform darwin/arm64 --destination "$PWD/data/tools" --install --allow-homebrew
```

可用 `--tools suo5,chisel,httpx` 选择子集，或 `--host-only` 排除目标端文件。
下载的官方归档先核对固定 SHA256，再检查可识别的二进制平台。
已管理的相同文件重跑跳过，用户新增或修改的冲突文件会要求处理，不直接覆盖。
下载临时目录随操作清理；`tools-lock.json` 记录来源与真实解包摘要，`tools-manifest.local.json` 供检查与注册使用。

## Python 工具

`python-tools-sources.json` 固定工具及必要 Git 依赖的源码提交。含源码构建的 wheelhouse 准备只允许在原生 Mac ARM64 GitHub Actions 中执行：

```sh
python3 packaging/prepare-python-tools.py --build-wheelhouse --wheelhouse WHEELHOUSE --python PYTHON311
```

正式标签的 Release 附带独立可选的 `artex-python-tools-版本-darwin-arm64-py311.zip`，并与平台 ZIP 一起记录在 `SHA256SUMS` 中。下载匹配版本、按清单校验并解压后，将其中 `python-tools-darwin-arm64-py311` 目录作为 `WHEELHOUSE`。该附件包含依赖 wheel、固定上游脚本及来源索引。

维护者手动运行 `release` 工作流时可启用 `prepare_macos_tools`，云端会构建 wheelhouse 并验收命令帮助和 Penelope 本机回环 MCP；手动分支构建仍可从同次工作流的 `python-tools-darwin-arm64-py311` artifact 取安装材料。正式 Release 附件长期保留，不依赖该短期 artifact。

本机使用已有 Python 3.11 安装：

```sh
python3 packaging/prepare-python-tools.py --install --destination "$PWD/data/tools" --wheelhouse WHEELHOUSE --python PYTHON311
```

本机安装严格使用离线 wheel、固定摘要和独立环境，不运行源码构建。
NetExec 使用固定 Git 依赖制作的 wheel；Penelope 使用包含 MCP 接口的固定官方提交，不使用缺少该接口的旧正式标签。
其受管数据保存在 ARTEX 目录，保留显式环境覆盖，不修改用户 HOME 或全局 PATH。
`python-tools-lock.json` 和 `python-tools-manifest.local.json` 保存可审计记录。

## 注册和自检

先检查注册计划，确认后使用 `--apply` 新增工具声明：

```sh
python3 packaging/register-tools.py --data "$PWD/data" --manifest "$PWD/data/tools/tools-manifest.local.json" --manifest "$PWD/data/tools/python-tools-manifest.local.json" --plan
python3 packaging/register-tools.py --data "$PWD/data" --manifest "$PWD/data/tools/tools-manifest.local.json" --manifest "$PWD/data/tools/python-tools-manifest.local.json" --apply
./artex doctor
```

注册仅添加缺失的 `external_*`、`kind=shell` 环境声明，默认绑定 worker，不覆盖已有条目、Agent 权限或审批列表。
工具提示使用绝对路径，防止 Python HTTPX 与 ProjectDiscovery httpx 同名冲突。
Nuclei 使用实例内模板目录；模板存在不代表所有检测均适用，应按任务范围选择。
完整性与平台检查、命令帮助验收、回环 MCP 验收和真实目标功能验证分别记录，不互相替代。
