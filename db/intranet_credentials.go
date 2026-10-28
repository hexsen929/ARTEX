// credentials.go 是凭据一等实体的落库层(schema.sql §K,内网渗透期 2,
// INTRANET-PIVOT-DESIGN.md §4.4)。secret 用 AES-GCM 加密存储:与 sessions 同款
// 派生方式(jwt.key → SHA-256 域分离)但不同域标签,两个子系统密钥互相独立。
package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// 凭据类型全集(schema CHECK 与 Go 校验同源)。
const (
	CredPassword = "password"
	CredHashNT   = "hash_nt"
	CredHashLM   = "hash_lm"
	CredHashSHA1 = "hash_sha1"
	CredTicket   = "ticket"
	CredSSHKey   = "ssh_key"
	CredToken    = "token"
)

// CredentialRecord 是 credentials 表的一行。Secret 在 Go 层是明文;Create 加密、
// Get/ListByTask 解密,密钥不出 CredentialStore。
type CredentialVerificationEvent struct {
	Status   string    `json:"status"`
	At       time.Time `json:"at"`
	Method   string    `json:"method"`
	Evidence string    `json:"evidence"`
	By       string    `json:"by"`
}

type CredentialRecord struct {
	ID                   int64                         `json:"id"`
	TaskID               int64                         `json:"task_id"`
	HostAssetID          int64                         `json:"host_asset_id,omitempty"`
	Username             string                        `json:"username"`
	CredType             string                        `json:"cred_type"`
	Secret               string                        `json:"-"` // Never serialize a store record's decrypted secret.
	Domain               string                        `json:"domain,omitempty"`
	Source               string                        `json:"source"`
	Target               string                        `json:"target"`
	SourceURL            string                        `json:"source_url"`
	SourceIntentID       int64                         `json:"source_intent_id"`
	SourceExplorationID  int64                         `json:"source_exploration_id"`
	Verified             bool                          `json:"verified"`
	VerificationStatus   string                        `json:"verification_status"`
	VerificationAt       *time.Time                    `json:"verification_at"`
	VerificationMethod   string                        `json:"verification_method"`
	VerificationEvidence string                        `json:"verification_evidence"`
	VerificationBy       string                        `json:"verification_by"`
	VerificationHistory  []CredentialVerificationEvent `json:"verification_history"`
	CreatedAt            time.Time                     `json:"created_at"`
	// Display-only fields are hydrated from the currently available task/asset.
	TaskName      string `json:"task_name"`
	TaskAvailable bool   `json:"task_available"`
	TargetLabel   string `json:"target_label"`
	TargetURL     string `json:"target_url"`
}

var (
	ErrCredentialNotFound                   = errors.New("credential not found")
	ErrCredentialUnavailable                = errors.New("credential task unavailable")
	ErrCredentialMetadataInvalid            = errors.New("invalid credential metadata")
	ErrCredentialVerificationInvalid        = errors.New("invalid credential verification")
	ErrCredentialVerificationContainsSecret = errors.New("credential verification contains secret")
)

// NormalizeCredentialMetadata keeps authentication domains distinct from source
// targets. URLs are navigation metadata, never replayable authenticated requests:
// userinfo is rejected and query/fragment are discarded before persistence.
func NormalizeCredentialMetadata(target, domain, sourceURL string) (string, string, string, error) {
	target, domain, sourceURL = strings.TrimSpace(target), strings.TrimSpace(domain), strings.TrimSpace(sourceURL)
	for _, value := range []string{target, domain, sourceURL} {
		if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return "", "", "", ErrCredentialMetadataInvalid
		}
	}
	if len(target) > 2048 || len(domain) > 255 || len(sourceURL) > 8192 {
		return "", "", "", ErrCredentialMetadataInvalid
	}
	if target != "" {
		if strings.Contains(target, "://") {
			var err error
			target, err = safeCredentialURL(target)
			if err != nil {
				return "", "", "", err
			}
		} else {
			u, err := url.Parse("//" + target)
			if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(target, " \\/?#") {
				return "", "", "", ErrCredentialMetadataInvalid
			}
		}
	}
	if sourceURL != "" {
		var err error
		sourceURL, err = safeCredentialURL(sourceURL)
		if err != nil {
			return "", "", "", err
		}
	}
	return target, domain, sourceURL, nil
}

func safeCredentialURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || strings.ContainsAny(u.Host, " \\\t\r\n") {
		return "", ErrCredentialMetadataInvalid
	}
	u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = "", "", "", false
	return u.String(), nil
}

func NormalizeCredentialVerification(status, method, evidence string) (string, string, string, error) {
	status, method, evidence = strings.TrimSpace(status), strings.TrimSpace(method), strings.TrimSpace(evidence)
	if status != "unverified" && status != "verified" && status != "failed" {
		return "", "", "", ErrCredentialVerificationInvalid
	}
	if len(method) > 512 || len(evidence) > 8192 || !utf8.ValidString(method) || !utf8.ValidString(evidence) || strings.IndexFunc(method, unicode.IsControl) >= 0 || strings.ContainsRune(evidence, '\x00') {
		return "", "", "", ErrCredentialVerificationInvalid
	}
	if status == "unverified" {
		return status, "", "", nil
	}
	if method == "" || evidence == "" {
		return "", "", "", ErrCredentialVerificationInvalid
	}
	return status, method, evidence, nil
}

// ValidCredType 校验凭据类型。
func ValidCredType(t string) bool {
	switch t {
	case CredPassword, CredHashNT, CredHashLM, CredHashSHA1, CredTicket, CredSSHKey, CredToken:
		return true
	}
	return false
}

// CredentialSecretKey 由 master key(jwt.key 内容)派生凭据加密密钥:与
// SessionSecretKey 同款 SHA-256 域分离,但域标签不同("artex/credentials/v1"),
// 两个子系统的密文互不通用。
func CredentialSecretKey(master []byte) []byte {
	sum := sha256.Sum256(append([]byte("artex/credentials/v1\x00"), master...))
	return sum[:]
}

// CredentialStore 是 credentials 表的访问层。key 为 32 字节 AES 密钥(经
// CredentialSecretKey 派生);key 缺失时加解密直接报错(诚实失败,不落明文)。
type CredentialStore struct {
	d   *DB
	key []byte
}

// NewCredentialStore 构造访问层。masterKey 传 jwt.key 内容;内部自行派生。
func NewCredentialStore(d *DB, masterKey []byte) *CredentialStore {
	var key []byte
	if len(masterKey) > 0 {
		key = CredentialSecretKey(masterKey)
	}
	return &CredentialStore{d: d, key: key}
}

func (s *CredentialStore) encrypt(plain string) (string, error) {
	if len(s.key) != 32 {
		return "", fmt.Errorf("凭据加密密钥未初始化,拒绝明文落库")
	}
	gcm, err := newGCM(s.key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (s *CredentialStore) decrypt(stored string) (string, error) {
	if len(stored) == 0 {
		return "", nil
	}
	if len(s.key) != 32 {
		return "", fmt.Errorf("凭据加密密钥未初始化,无法解密")
	}
	if !hasPrefix(stored, secretPrefix) {
		return "", fmt.Errorf("未知 secret 格式(缺 gcm1: 前缀),拒绝按明文读取")
	}
	raw, err := base64.StdEncoding.DecodeString(stored[len(secretPrefix):])
	if err != nil {
		return "", fmt.Errorf("secret base64 解码失败: %w", err)
	}
	gcm, err := newGCM(s.key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("secret 密文过短")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("secret 解密失败(密钥不符或数据被篡改): %w", err)
	}
	return string(plain), nil
}

// Create 插入一条凭据并返回主键。Secret 加密存储。
func (s *CredentialStore) Create(ctx context.Context, rec *CredentialRecord) (int64, error) {
	if rec == nil || rec.TaskID <= 0 {
		return 0, ErrCredentialMetadataInvalid
	}
	if !ValidCredType(rec.CredType) || rec.Secret == "" {
		return 0, ErrCredentialMetadataInvalid
	}
	target, domain, sourceURL, err := NormalizeCredentialMetadata(rec.Target, rec.Domain, rec.SourceURL)
	if err != nil {
		return 0, err
	}
	if rec.HostAssetID < 0 || rec.SourceIntentID < 0 || rec.SourceExplorationID < 0 {
		return 0, ErrCredentialMetadataInvalid
	}
	if rec.HostAssetID > 0 {
		var belongs bool
		if err := s.d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE id=$1 AND $2=ANY(task_ids))`, rec.HostAssetID, rec.TaskID).Scan(&belongs); err != nil {
			return 0, err
		}
		if !belongs {
			return 0, ErrCredentialMetadataInvalid
		}
	}
	if rec.SourceIntentID > 0 || rec.SourceExplorationID > 0 {
		var valid bool
		if err := s.d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks t WHERE t.id=$1 AND t.deleted_at IS NULL AND t.exploration_id=$2 AND ($3=0 OR EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.id=$3 AND n.exploration_id=t.exploration_id)))`, rec.TaskID, rec.SourceExplorationID, rec.SourceIntentID).Scan(&valid); err != nil {
			return 0, err
		}
		if !valid {
			return 0, ErrCredentialMetadataInvalid
		}
	}
	enc, err := s.encrypt(rec.Secret)
	if err != nil {
		return 0, err
	}
	status := "unverified"
	if rec.Verified {
		status = "verified"
	}
	var id int64
	err = s.d.QueryRowContext(ctx, `INSERT INTO credentials(task_id, host_asset_id, username, cred_type, secret, domain, source, verified, target, source_url, source_intent_id, source_exploration_id, verification_status)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		rec.TaskID, nilIfZero(rec.HostAssetID), rec.Username, rec.CredType, enc, domain, rec.Source, rec.Verified, target, sourceURL, rec.SourceIntentID, rec.SourceExplorationID, status).Scan(&id)
	return id, err
}

const credentialCols = `c.id, c.task_id, COALESCE(c.host_asset_id,0), c.username, c.cred_type, c.secret,
 c.domain, c.source, c.verified, c.created_at, c.target, c.source_url, c.source_intent_id, c.source_exploration_id,
 c.verification_status, c.verification_at, c.verification_method, c.verification_evidence, c.verification_by, c.verification_history,
 COALESCE(t.name,''), (t.id IS NOT NULL AND t.deleted_at IS NULL AND t.archived_at IS NULL AND NOT EXISTS(SELECT 1 FROM task_archives ar WHERE ar.task_id=t.id AND ar.state IN ('archive_queued','archiving','restore_queued','restoring','delete_queued','deleting'))),
 COALESCE(NULLIF(a.url,''),NULLIF(a.domain,''),a.ip,''), COALESCE(a.url,'')`
const credentialFrom = ` FROM credentials c LEFT JOIN tasks t ON t.id=c.task_id
 LEFT JOIN assets a ON a.id=c.host_asset_id AND c.task_id=ANY(a.task_ids)`

func (s *CredentialStore) scan(row interface{ Scan(...any) error }) (*CredentialRecord, error) {
	var r CredentialRecord
	var enc, assetLabel, assetURL string
	var history []byte
	if err := row.Scan(&r.ID, &r.TaskID, &r.HostAssetID, &r.Username, &r.CredType, &enc,
		&r.Domain, &r.Source, &r.Verified, &r.CreatedAt, &r.Target, &r.SourceURL, &r.SourceIntentID, &r.SourceExplorationID,
		&r.VerificationStatus, &r.VerificationAt, &r.VerificationMethod, &r.VerificationEvidence, &r.VerificationBy, &history,
		&r.TaskName, &r.TaskAvailable, &assetLabel, &assetURL); err != nil {
		return nil, err
	}
	plain, err := s.decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("credential %d: %w", r.ID, err)
	}
	r.Secret = plain
	r.Verified = r.VerificationStatus == "verified"
	r.VerificationHistory = []CredentialVerificationEvent{}
	if err := json.Unmarshal(history, &r.VerificationHistory); err != nil {
		return nil, errors.New("invalid credential verification history")
	}
	if r.VerificationHistory == nil {
		r.VerificationHistory = []CredentialVerificationEvent{}
	}
	r.TargetLabel = r.Target
	if r.TargetLabel == "" {
		r.TargetLabel = assetLabel
		if strings.Contains(assetLabel, "://") {
			// An asset URL may contain authentication query parameters. Labels
			// follow the same stripping rules as links, not the raw asset URL.
			r.TargetLabel, _ = safeCredentialURL(assetLabel)
		}
	}
	for _, candidate := range []string{r.Target, assetURL} {
		if candidate == "" {
			continue
		}
		if safe, err := safeCredentialURL(candidate); err == nil {
			u, _ := url.Parse(safe)
			// Do not attach a different asset/source host to an explicit target.
			if r.Target != "" && !strings.Contains(r.Target, "://") && !strings.EqualFold(u.Host, r.Target) {
				continue
			}
			r.TargetURL = safe
			break
		}
	}
	return &r, nil
}

func (s *CredentialStore) Get(ctx context.Context, id int64) (*CredentialRecord, error) {
	r, err := s.scan(s.d.QueryRowContext(ctx, `SELECT `+credentialCols+credentialFrom+` WHERE c.id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *CredentialStore) list(ctx context.Context, taskID int64) ([]*CredentialRecord, error) {
	query := `SELECT ` + credentialCols + credentialFrom
	var args []any
	if taskID > 0 {
		query += ` WHERE c.task_id=$1`
		args = append(args, taskID)
	}
	rows, err := s.d.QueryContext(ctx, query+` ORDER BY c.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CredentialRecord{}
	for rows.Next() {
		r, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *CredentialStore) ListByTask(ctx context.Context, taskID int64) ([]*CredentialRecord, error) {
	return s.list(ctx, taskID)
}
func (s *CredentialStore) ListAll(ctx context.Context) ([]*CredentialRecord, error) {
	return s.list(ctx, 0)
}

// lockCredentialTask shares the archive queue's task-row lock order. The check
// and mutation are in one transaction, in addition to the runtime drain fence.
func (s *CredentialStore) lockCredentialTask(ctx context.Context, tx *sql.Tx, id int64) error {
	var taskID int64
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM credentials WHERE id=$1`, id).Scan(&taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCredentialNotFound
		}
		return err
	}
	var available bool
	err := tx.QueryRowContext(ctx, `SELECT archived_at IS NULL FROM tasks WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, taskID).Scan(&available)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCredentialUnavailable
	}
	if err != nil {
		return err
	}
	if !available {
		return ErrCredentialUnavailable
	}
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_archives WHERE task_id=$1 AND state IN ('archive_queued','archiving','restore_queued','restoring','delete_queued','deleting'))`, taskID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return ErrCredentialUnavailable
	}
	return nil
}

func (s *CredentialStore) UpdateMetadata(ctx context.Context, id int64, target, domain, sourceURL string) error {
	return s.PatchMetadata(ctx, id, &target, &domain, &sourceURL)
}

// PatchMetadata merges under the row lock so disjoint concurrent PATCH requests
// do not silently overwrite each other's changes with an earlier HTTP snapshot.
func (s *CredentialStore) PatchMetadata(ctx context.Context, id int64, target, domain, sourceURL *string) error {
	tx, err := s.d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockCredentialTask(ctx, tx, id); err != nil {
		return err
	}
	var currentTarget, currentDomain, currentSourceURL string
	if err := tx.QueryRowContext(ctx, `SELECT target,domain,source_url FROM credentials WHERE id=$1 FOR UPDATE`, id).Scan(&currentTarget, &currentDomain, &currentSourceURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCredentialNotFound
		}
		return err
	}
	if target != nil {
		currentTarget = *target
	}
	if domain != nil {
		currentDomain = *domain
	}
	if sourceURL != nil {
		currentSourceURL = *sourceURL
	}
	currentTarget, currentDomain, currentSourceURL, err = NormalizeCredentialMetadata(currentTarget, currentDomain, currentSourceURL)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE credentials SET target=$2,domain=$3,source_url=$4 WHERE id=$1`, id, currentTarget, currentDomain, currentSourceURL)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCredentialNotFound
	}
	return tx.Commit()
}

func (s *CredentialStore) SetVerification(ctx context.Context, id int64, status, method, evidence, by string) error {
	status, method, evidence, err := NormalizeCredentialVerification(status, method, evidence)
	if err != nil {
		return err
	}
	if len(by) > 512 || !utf8.ValidString(by) || strings.IndexFunc(by, unicode.IsControl) >= 0 {
		return ErrCredentialVerificationInvalid
	}
	tx, err := s.d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockCredentialTask(ctx, tx, id); err != nil {
		return err
	}
	var encrypted string
	if err := tx.QueryRowContext(ctx, `SELECT secret FROM credentials WHERE id=$1 FOR UPDATE`, id).Scan(&encrypted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCredentialNotFound
		}
		return err
	}
	plain, err := s.decrypt(encrypted)
	if err != nil {
		return err
	}
	if utf8.RuneCountInString(plain) >= 4 && (strings.Contains(method, plain) || strings.Contains(evidence, plain)) {
		return ErrCredentialVerificationContainsSecret
	}
	if err := updateCredentialVerification(ctx, tx, id, status, method, evidence, by, false); err != nil {
		return err
	}
	return tx.Commit()
}

type credentialExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func updateCredentialVerification(ctx context.Context, exec credentialExecer, id int64, status, method, evidence, by string, legacy bool) error {
	res, err := exec.ExecContext(ctx, `UPDATE credentials SET verification_status=$2,verified=($2='verified'),
 verification_at=CASE WHEN $2='unverified' OR $6 THEN NULL ELSE now() END,
 verification_method=$3,verification_evidence=$4,verification_by=CASE WHEN $2='unverified' THEN '' ELSE $5 END,
 verification_history=verification_history || jsonb_build_array(jsonb_build_object('status',$2::text,'at',now(),'method',$3::text,'evidence',$4::text,'by',$5::text)) WHERE id=$1`, id, status, method, evidence, by, legacy)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCredentialNotFound
	}
	return nil
}

// SetVerified is retained for internal compatibility. It synchronizes both
// representations atomically but makes no claim about method/evidence/time.
func (s *CredentialStore) SetVerified(ctx context.Context, id int64, verified bool) error {
	status := "unverified"
	if verified {
		status = "verified"
	}
	return updateCredentialVerification(ctx, s.d, id, status, "", "", "compatibility", true)
}

// Delete 删除一条。
func (s *CredentialStore) Delete(ctx context.Context, id int64) error {
	_, err := s.d.ExecContext(ctx, `DELETE FROM credentials WHERE id=$1`, id)
	return err
}

// MaskSecret 脱敏展示:只露前后各 2 字符,其余掩码;过短(≤4 字符)全掩码。
func MaskSecret(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return "****"
	}
	return string(r[:2]) + "****" + string(r[len(r)-2:])
}
