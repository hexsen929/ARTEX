---
name: shodan
description: 用 Shodan 做情报侦察，接进 Web 渗透攻击链。当需要查目标暴露的服务/端口/组件版本/证书、扩展关联资产、找认证前可达入口、或按指纹/favicon/标题全网搜同类资产时调用。使用前需在运行环境设置 SHODAN_API_KEY。
allowed-tools: Bash(${SKILL_DIR}/scripts/shodan:*) Bash(curl:*)
---

# Shodan 情报侦察

运行 ARTEX 前设置环境变量 `SHODAN_API_KEY`。脚本未配置密钥时直接报错，不发送请求；不要把密钥写进技能或提交到仓库：

```
${SKILL_DIR}/scripts/shodan <子命令> [参数]
```

## 子命令

| 命令 | 用途 |
| --- | --- |
| `info` | 查 API 额度/套餐（先确认 key 可用、还剩多少 query 额度） |
| `host <ip>` | 某 IP 的全部开放端口 / 服务 / banner / 组件版本 / 已知 CVE（**不消耗 query 额度**） |
| `count '<query>'` | 只返回命中数 + 分面统计，**不消耗记录额度**，先摸量再决定要不要 search |
| `search '<query>' [页]` | Shodan 搜索（**消耗 query 额度**，每页 100 条） |
| `domain <domain>` | 子域名 + DNS 记录 |
| `resolve h1,h2` | 域名 → IP |
| `reverse ip1,ip2` | IP → 域名 |
| `myip` | 出口 IP |

## 常用搜索过滤器（query 语法）

- `hostname:example.com`、`ssl:example.com`（证书 CN/SAN 命中，常能挖出隐藏资产）
- `http.title:"admin"`、`http.html:"关键字"`、`http.favicon.hash:<hash>`（按 favicon 找同款后台）
- `product:nginx`、`port:8080`、`org:"公司名"`、`net:1.2.3.0/24`、`vuln:CVE-2021-xxxxx`
- 组合：`ssl:example.com http.title:"login" -port:443`

## 接进攻击链的用法

1. **扩面**：`domain` + `ssl:` 搜索把目标的子域名、关联 IP、同证书资产捞全，回填资产图再派深打意图。
2. **定入口**：`host <ip>` 看每个 IP 真实开放的服务/中间件/版本和已知 CVE —— 认证前可达的后台、管理端口、老组件就是突破口。
3. **省额度**：先 `count` 摸量、`host` 查主机（都不耗 query 额度），确有必要再 `search`（dev 套餐 query 额度有限）。

## 纪律

- 只对授权范围内的目标做情报查询；Shodan 是**被动情报**（查它已有的扫描库），不直接打目标。
- 结果里的新资产用 `insert_assets` 回填资产图；据此得出的判断用 `record_fact` 记录（标 confidence）。
- key 失效或额度耗尽（`info` 可查）时，记一条 inferred 事实说明"Shodan 额度/key 需处理"，改走其它侦察路子。
