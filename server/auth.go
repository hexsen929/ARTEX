package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	jwtKeyFilename = "jwt.key"
	authPassKey    = "auth.password_hash"
	// authKeyVersionKey 是 settings 里的密钥版本号:改密/重置密码时 +1,JWT 与
	// refresh token 都带签发时的版本,比对不符即视为吊销(F6 改密吊销)。
	authKeyVersionKey = "auth.key_version"
	// access token 2 小时;refresh token 14 天、随机 32 字节(非 JWT),SHA-256
	// 哈希存 settings(HttpOnly cookie 下发,JS 不可读)。
	jwtTTL            = 2 * time.Hour
	refreshTTL        = 14 * 24 * time.Hour
	refreshCookieName = "artex_refresh"
	refreshCookiePath = "/api/auth"
	sseTicketTTL      = time.Minute
	keyChars          = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// Match setup validation and bcrypt's byte limit.
	minPasswordRunes = 8
	maxPasswordBytes = 72
)

// errDataSourceUnavailable 是密码相关读操作失败时统一的回复。这些 handler 绝不能
// 把"读不到"当成"没有设置"：authInit 曾因此在数据库报错时放行，让未认证请求覆盖
// 掉已有的管理员密码。
const errDataSourceUnavailable = "数据源暂时不可用，请稍后重试"

// validatePassword 返回空串表示通过，否则返回可直接展示给用户的中文原因。
func validatePassword(pw string) string {
	if utf8.RuneCountInString(pw) < minPasswordRunes {
		return fmt.Sprintf("密码长度至少 %d 位", minPasswordRunes)
	}
	if len(pw) > maxPasswordBytes {
		return fmt.Sprintf("密码长度不能超过 %d 字节", maxPasswordBytes)
	}
	return ""
}

// loadOrCreateJWTKey reads the 32-byte signing key from keyDir/jwt.key. keyDir is
// the project base dir (next to the executable), NOT the browsable workspace root
// (dataDir) — the signing key must never be listable/downloadable via the file
// manager. Legacy installs kept it at dataDir/jwt.key; if present there and not yet
// at the new location, it is migrated (key preserved, so sessions stay valid) and
// the old file removed so it disappears from the workspace. On first run a random
// key is generated and persisted.
func loadOrCreateJWTKey(keyDir, dataDir string, legacyKeyDirs ...string) ([]byte, error) {
	path := filepath.Join(keyDir, jwtKeyFilename)
	if data, err := readSigningKey(path); err == nil {
		return data, nil
	} else if !os.IsNotExist(err) {
		return nil, err // never replace an existing invalid/unreadable key
	}
	// Explicit legacy base dirs take precedence over the historical browsable
	// data dir. Preserve bytes: sessions and encrypted credentials use this key.
	for _, dir := range append(legacyKeyDirs, dataDir) {
		legacy := filepath.Join(dir, jwtKeyFilename)
		if legacy == path {
			continue
		}
		data, err := readSigningKey(legacy)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		stored, err := writeSigningKeyIfAbsent(path, data)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(stored, data) {
			return nil, fmt.Errorf("JWT key changed during migration")
		}
		if filepath.Clean(dir) == filepath.Clean(dataDir) {
			if err := os.Remove(legacy); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("remove legacy workspace key: %w", err)
			}
		}
		log.Printf("[auth] JWT key 已迁移到 %s（保留原密钥）", path)
		return stored, nil
	}
	data, err := loadOrCreateSigningKey(path)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func readSigningKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) < 32 {
		return nil, fmt.Errorf("existing signing key is invalid: %s", path)
	}
	return data, nil
}

func loadOrCreateSigningKey(path string) ([]byte, error) {
	if data, err := readSigningKey(path); err == nil {
		return data, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	buf := make([]byte, 32)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(keyChars))))
		if err != nil {
			return nil, fmt.Errorf("generate signing key: %w", err)
		}
		buf[i] = keyChars[n.Int64()]
	}
	return writeSigningKeyIfAbsent(path, buf)
}

func writeSigningKeyIfAbsent(path string, data []byte) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".signing-key-*")
	if err != nil {
		return nil, fmt.Errorf("create signing key: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return nil, fmt.Errorf("persist signing key: %w", err)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	// Publish a fully written key atomically, without replacing a winner from
	// another startup process. Readers never observe an empty in-progress file.
	if err = os.Link(f.Name(), path); os.IsExist(err) {
		return readSigningKey(path)
	} else if err != nil {
		return nil, fmt.Errorf("publish signing key: %w", err)
	}
	return append([]byte(nil), data...), nil
}

// authClaims 是 access token 的 JWT claims:ver 为签发时的 auth.key_version,
// 校验时与当前版本比对,不一致即 401(改密吊销全部已签发凭证)。
type authClaims struct {
	Ver int64 `json:"ver"`
	jwt.RegisteredClaims
}

// signJWT issues a 2-hour HS256 access token for user ARTEX, stamped with the
// current auth key version.
func signJWT(key []byte, versions ...int64) (string, error) {
	var ver int64
	if len(versions) > 0 {
		ver = versions[0]
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, authClaims{
		Ver: ver,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "ARTEX",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(jwtTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}).SignedString(key)
}

// parseAccessToken validates signature (HMAC alg whitelist), expiry, and
// returns the claims. Version checking is the caller's job (needs the store).
func parseAccessToken(tokenStr string, key []byte) (*authClaims, error) {
	claims := &authClaims{}
	if _, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		return key, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired(), jwt.WithSubject("ARTEX")); err != nil {
		return nil, err
	}
	return claims, nil
}

// verifyJWT keeps the existing signature-only helper used by Basic Auth tests.
// Request authentication additionally checks the persisted key version below.
func verifyJWT(tokenStr string, key []byte) bool {
	_, err := parseAccessToken(tokenStr, key)
	return err == nil
}

// validAccessToken reports whether tok is a well-formed, non-expired access
// token whose version matches the current auth key version. 版本读取失败
// fail-closed(视为吊销);无持久层(测试/降级)时只做签名+过期校验。
func (s *Server) validAccessToken(tok string) bool {
	claims, err := parseAccessToken(tok, s.jwtKey)
	if err != nil {
		return false
	}
	kv := s.authSettings()
	if kv == nil {
		return true
	}
	ver, ok := authKeyVersion(kv)
	if !ok {
		return false
	}
	return claims.Ver == ver
}

// extractToken reads the access token from the Authorization: Bearer header.
// (SSE connections use one-time ?ticket= instead — see sseTicketStore.)
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// requireAuth validates Bearer access tokens or one-use SSE tickets.
// /api/auth/*, /api/health and the Basic Auth gate check remain exempt.
func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/auth/") || p == "/api/health" || p == "/api/basic-auth/check" {
			h.ServeHTTP(w, r)
			return
		}
		if tok := extractToken(r); tok != "" {
			if !s.validAccessToken(tok) {
				writeErr(w, 401, "token 无效或已过期")
				return
			}
			h.ServeHTTP(w, r)
			return
		}
		if t := r.URL.Query().Get("ticket"); t != "" && r.Method == http.MethodGet && isSSEPath(p) && s.tickets.consume(t) {
			h.ServeHTTP(w, r)
			return
		}
		writeErr(w, 401, "未授权")
	})
}

func isSSEPath(path string) bool {
	switch path {
	case "/api/logs/stream", "/api/update/stream", "/api/exploration/activity/stream":
		return true
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 4 && path == "/"+strings.Join(parts, "/") && parts[0] == "api" && parts[1] == "side-questions" && parts[2] != "" && parts[3] == "events"
}

// ---------- refresh token(随机 32 字节,哈希存 settings) ----------

// authKV 是 auth 持久层的最小接口:*db.DB 天然满足,测试注入内存实现。
type authKV interface {
	GetSetting(key string) (value string, ok bool, err error)
	SetSetting(key, value string) error
	DeleteSetting(key string) error
}

// authSettings returns the auth KV store: the test-injected one if set,
// otherwise the PG handle (nil when PG is unavailable).
func (s *Server) authSettings() authKV {
	if s.authkv != nil {
		return s.authkv
	}
	if s.m != nil && s.m.pg != nil {
		return s.m.pg
	}
	return nil
}

// refreshRecord 是 settings 里 refresh token 哈希对应的内容。
type refreshRecord struct {
	Exp int64 `json:"exp"` // unix 过期时间
	Ver int64 `json:"ver"` // 签发时的 auth.key_version
}

// authKeyVersion reads the current key version (default 0 when unset);
// ok=false 表示存储层错误(调用方应 fail-closed)。
func authKeyVersion(kv authKV) (int64, bool) {
	v, ok, err := kv.GetSetting(authKeyVersionKey)
	if err != nil {
		return 0, false
	}
	if !ok {
		return 0, true
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// bumpAuthKeyVersion 把版本号 +1,返回新版本(改密后调用,吊销全部旧凭证)。
func bumpAuthKeyVersion(kv authKV) (int64, error) {
	ver, ok := authKeyVersion(kv)
	if !ok {
		return 0, fmt.Errorf("read auth key version")
	}
	ver++
	if err := kv.SetSetting(authKeyVersionKey, strconv.FormatInt(ver, 10)); err != nil {
		return 0, err
	}
	return ver, nil
}

func hashRefreshToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func refreshSettingKey(hash string) string { return "auth.refresh." + hash }

// issueRefreshToken 生成随机 32 字节 refresh token,哈希落 settings,返回原文。
func issueRefreshToken(kv authKV, ver int64) (string, error) {
	tok, err := randomGateToken(32)
	if err != nil {
		return "", err
	}
	rec, _ := json.Marshal(refreshRecord{Exp: time.Now().Add(refreshTTL).Unix(), Ver: ver})
	if err := kv.SetSetting(refreshSettingKey(hashRefreshToken(tok)), string(rec)); err != nil {
		return "", err
	}
	return tok, nil
}

// validateRefreshToken 校验 refresh token:存在、未过期、版本匹配。
func validateRefreshToken(kv authKV, tok string, curVer int64) (refreshRecord, bool) {
	v, ok, err := kv.GetSetting(refreshSettingKey(hashRefreshToken(tok)))
	if err != nil || !ok {
		return refreshRecord{}, false
	}
	var rec refreshRecord
	if json.Unmarshal([]byte(v), &rec) != nil {
		return refreshRecord{}, false
	}
	if time.Now().Unix() >= rec.Exp || rec.Ver != curVer {
		return refreshRecord{}, false
	}
	return rec, true
}

// setRefreshCookie 下发 HttpOnly refresh cookie:Path 收窄到 /api/auth(只有
// refresh/logout 会带它),SameSite=Lax,https 时加 Secure。
func setRefreshCookie(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    tok,
		Path:     refreshCookiePath,
		MaxAge:   int(refreshTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
}

func clearRefreshCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
}

// issueSession 登录/初始化成功后统一调用:签 access token(带当前版本)+
// 下发 refresh cookie,响应体只含 access token。
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, kv authKV) {
	ver, ok := authKeyVersion(kv)
	if !ok {
		writeErr(w, 500, "读取密钥版本失败")
		return
	}
	access, err := signJWT(s.jwtKey, ver)
	if err != nil {
		writeErr(w, 500, "token 生成失败")
		return
	}
	rt, err := issueRefreshToken(kv, ver)
	if err != nil {
		writeErr(w, 500, "refresh token 生成失败: "+err.Error())
		return
	}
	setRefreshCookie(w, r, rt)
	writeJSON(w, 200, map[string]any{"token": access})
}

// ---------- SSE 一次性票据(内存存,60s 有效,用后即焚) ----------

type sseTicketStore struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func (s *sseTicketStore) issue() (string, error) {
	tok, err := randomGateToken(16)
	if err != nil {
		return "", err
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]time.Time{}
	}
	// 防内存膨胀:票据过多时顺手清扫已过期的。
	if len(s.m) > 10000 {
		for k, exp := range s.m {
			if now.After(exp) {
				delete(s.m, k)
			}
		}
	}
	if len(s.m) >= 10000 {
		return "", fmt.Errorf("too many outstanding SSE tickets")
	}
	s.m[tok] = now.Add(sseTicketTTL)
	return tok, nil
}

func (s *sseTicketStore) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m = nil
}

// consume 核销票据:存在且未过期才放行,无论成功与否票据都只此一次。
func (s *sseTicketStore) consume(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[tok]
	if !ok {
		return false
	}
	delete(s.m, tok)
	return time.Now().Before(exp)
}

// POST /api/sse-ticket — 需 access token(requireAuth 不过滤该路径);
// 返回 60s 一次性 ticket,前端 EventSource 用 ?ticket= 拼接。
func (s *Server) sseTicket(w http.ResponseWriter, r *http.Request) {
	t, err := s.tickets.issue()
	if err != nil {
		writeErr(w, 500, "ticket 生成失败")
		return
	}
	writeJSON(w, 200, map[string]any{"ticket": t})
}

// GET /api/auth/status — reports whether the admin password has been initialised.
// 读失败必须回 503 而不是 initialized:false：前端在 initialized:false 时会把用户
// 送到 /setup 去设置密码（login/page.tsx），把数据库故障包装成 200 等于把用户往
// 覆盖已有密码的路上推。
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	hash, _, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	writeJSON(w, 200, map[string]any{"initialized": hash != ""})
}

// POST /api/auth/init — sets the password for the first time; rejected if already set.
func (s *Server) authInit(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	existing, _, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if existing != "" {
		writeErr(w, 403, "密码已设置")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil || req.Password == "" {
		writeErr(w, 400, "密码不能为空")
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "密码加密失败")
		return
	}
	// 用 INSERT ... ON CONFLICT DO NOTHING 而不是 upsert：上面那次 GetSetting 只是
	// 快速失败路径，真正"仅首次可设"的保证落在主键约束上。bcrypt 要跑几十毫秒，
	// 这期间别的请求完全可能先把密码设好，而读检查本身也可能因故障而失效。
	inserted, err := pg.InsertSettingIfAbsent(authPassKey, string(hash))
	if err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if !inserted {
		writeErr(w, 403, "密码已设置")
		return
	}
	s.issueSession(w, r, pg)
}

// POST /api/auth/change-password — changes the admin password. Requires a valid
// token (this route is under /api/auth/* which requireAuth exempts, so the token
// is validated here) AND the current password.
func (s *Server) authChangePassword(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if !s.validAccessToken(extractToken(r)) {
		writeErr(w, 401, "未授权")
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if req.NewPassword == "" {
		writeErr(w, 400, "新密码不能为空")
		return
	}
	if msg := validatePassword(req.NewPassword); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	hash, ok, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if !ok || hash == "" {
		writeErr(w, 403, "密码未初始化，请先设置密码")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.OldPassword)); err != nil {
		writeErr(w, 401, "当前密码错误")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "密码加密失败")
		return
	}
	// Persist the password change and token revocation atomically. A concurrent
	// change must not overwrite a password whose old value was already replaced.
	tx, err := pg.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), `UPDATE settings SET value=$1,updated_at=now() WHERE key=$2 AND value=$3`, string(newHash), authPassKey, hash)
	if err != nil {
		writeErr(w, 500, "密码更新失败")
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		writeErr(w, 409, "密码已被其他请求修改，请重新登录后重试")
		return
	}
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO settings(key,value) VALUES ($1,'1')
ON CONFLICT (key) DO UPDATE SET value=(settings.value::bigint+1)::text,updated_at=now()`, authKeyVersionKey); err != nil {
		writeErr(w, 500, "吊销旧凭证失败")
		return
	}
	if err = tx.Commit(); err != nil {
		writeErr(w, 500, "密码更新提交失败")
		return
	}
	s.tickets.clear()
	clearRefreshCookie(w, r)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/auth/refresh — 凭 HttpOnly refresh cookie 换新 access token,
// 并滑动续期 refresh(旧票作废,新票重置 14 天)。版本不符/过期/未知一律 401。
func (s *Server) authRefresh(w http.ResponseWriter, r *http.Request) {
	kv := s.authSettings()
	if kv == nil {
		writeErr(w, 503, "管理后台数据源(PostgreSQL)未连接")
		return
	}
	c, err := r.Cookie(refreshCookieName)
	if err != nil || c.Value == "" {
		writeErr(w, 401, "refresh token 缺失")
		return
	}
	ver, ok := authKeyVersion(kv)
	if !ok {
		writeErr(w, 500, "读取密钥版本失败")
		return
	}
	if _, ok := validateRefreshToken(kv, c.Value, ver); !ok {
		// Do not clear a possibly newer cookie set by a successful parallel
		// refresh in another tab. Explicit logout still clears it.
		writeErr(w, 401, "refresh token 无效或已过期")
		return
	}
	// Consume atomically before rotation; parallel refreshes must not both mint
	// new credentials from one old cookie. DB failures remain fail-closed.
	consumer, ok := kv.(interface {
		ConsumeSetting(string) (string, bool, error)
	})
	if !ok {
		writeErr(w, 503, "refresh token 存储不支持原子消费")
		return
	}
	raw, consumed, err := consumer.ConsumeSetting(refreshSettingKey(hashRefreshToken(c.Value)))
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	var consumedRecord refreshRecord
	if !consumed || json.Unmarshal([]byte(raw), &consumedRecord) != nil || consumedRecord.Ver != ver || time.Now().Unix() >= consumedRecord.Exp {
		writeErr(w, 401, "refresh token 无效或已过期")
		return
	}
	rt, err := issueRefreshToken(kv, ver)
	if err != nil {
		writeErr(w, 500, "refresh token 生成失败: "+err.Error())
		return
	}
	access, err := signJWT(s.jwtKey, ver)
	if err != nil {
		writeErr(w, 500, "token 生成失败")
		return
	}
	setRefreshCookie(w, r, rt)
	writeJSON(w, 200, map[string]any{"token": access})
}

// POST /api/auth/logout — 吊销当前 refresh token 并清 cookie。
// access token 是短寿命无状态凭证,前端自行丢弃即可。
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if kv := s.authSettings(); kv != nil {
		if c, err := r.Cookie(refreshCookieName); err == nil && c.Value != "" {
			if err := kv.DeleteSetting(refreshSettingKey(hashRefreshToken(c.Value))); err != nil {
				clearRefreshCookie(w, r)
				writeErr(w, 503, errDataSourceUnavailable)
				return
			}
		}
	}
	clearRefreshCookie(w, r)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/auth/login — validates username/password and returns a JWT.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if req.Username != "ARTEX" {
		writeErr(w, 401, "用户名或密码错误")
		return
	}
	hash, ok, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if !ok || hash == "" {
		writeErr(w, 403, "密码未初始化，请先设置密码")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		writeErr(w, 401, "用户名或密码错误")
		return
	}
	s.issueSession(w, r, pg)
}
