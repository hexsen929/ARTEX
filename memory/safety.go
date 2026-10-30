package memory

import (
	"encoding/base64"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// These checks intentionally reject a source rather than claiming that a regex
// can remove every secret. Callers may additionally compare known-secret
// fingerprints. No rejected content is included in errors or logs here.
var sensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)-----BEGIN[ A-Z0-9]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{12,}\b`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`(?i)https?://[^\s/@:]+:[^\s/@]+@`),
}

var (
	sensitiveAssignment = regexp.MustCompile(`(?i)(?:\b(?:api[ _-]?key|access[ _-]?token|refresh[ _-]?token|client[ _-]?secret|secret[ _-]?key|password|passwd|pwd|cookie|set-cookie|session[ _-]?cookie|authorization|proxy-authorization)\b|密码|口令|令牌|私钥)["']?\s*[:=：]\s*["']?([^\s,;"'<>}\]]+)`)
	bearerValue         = regexp.MustCompile(`(?i)\bBearer\s+([^\s,;"'<>}\]]{8,})`)
	basicValue          = regexp.MustCompile(`(?i)\bBasic\s+([A-Za-z0-9+/]+={0,2})`)
	internalMarker      = regexp.MustCompile(`(?i)(?:<\s*/?\s*(?:system|developer|assistant|memory|memory_context|memory-context|system-reminder)(?:\s|>|/)|\[\s*(?:/?INST|/?SYSTEM|/?DEVELOPER|/?MEMORY|im_start|im_end)\s*\]|<<\s*/?SYS\s*>>)`)
	// Scope/instruction changes of ARTEX are not the same thing as describing
	// a target's authorization flaw. Do not reject ordinary testing terminology.
	authorityChange   = regexp.MustCompile(`(?is)(?:(?:ignore|override|disregard)\b.{0,48}\b(?:instructions?|system\s+rules?|developer\s+rules?)|(?:bypass|disable|remove|expand|extend|override)\b.{0,32}\b(?:platform|artex|agent|task|allowed|registered|approved|authorized)\b.{0,24}\b(?:scope|permissions?|approval|guard|authorization|rules?|restrictions?)|(?:disable|remove|bypass)\b.{0,16}\b(?:scope\s+(?:checks?|enforcement)|tool\s+approvals?|safety\s+guards?)|(?:忽略|覆盖|无视).{0,12}(?:此前|先前|所有|系统|开发者|更高优先级).{0,12}(?:指令|规则|提示词)|(?:忽略|覆盖|绕过|关闭|取消|扩大|扩展).{0,16}(?:平台|系统|ARTEX|本任务|当前任务|代理|Agent|已授权|登记|已批准).{0,24}(?:指令|授权|范围|审批|规则|权限|限制)|(?:扩大|扩展|取消|关闭).{0,8}(?:授权范围|范围校验|范围限制|工具审批|审批校验|安全限制|安全检查)|(?:用户|管理员)(?:长期偏好|偏好|身份变更)|\b(?:user|operator|admin)\s+(?:always\s+prefers?|prefers?)\b|(?:^|\n)\s*(?:you\s+are\b|your\s+(?:role|identity)\b|你(?:现在)?是|你的(?:身份|角色)))`)
	confirmationClaim = regexp.MustCompile(`(?i)(?:\b(?:confirmed|verified|proven)\s+(?:vulnerability|finding)\b|已确认漏洞|已验证漏洞|确认存在漏洞)`)
	escapedCharacter  = regexp.MustCompile(`\\(?:u[0-9a-fA-F]{4}|x[0-9a-fA-F]{2})`)
	nativeFactID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_-]{0,127}$`)
	sharedURL         = regexp.MustCompile(`(?i)(?:\b[a-z][a-z0-9+.-]{1,20}://|\bwww\.)`)
	platformClaim     = regexp.MustCompile(`(?i)\b(?:macOS|darwin|arm64|aarch64|amd64|x86_64|linux|windows|ubuntu|debian|centos|rhel|alpine|freebsd|android|ios)\b`)
	accessClaim       = regexp.MustCompile(`(?i)管理员|超级用户|\badministrator\b|\bsuperuser\b|\badmin\b|已登录|已认证|需要登录|必须登录|登录后|登录状态|登录会话|有效会话|未登录|无需登录|匿名|\bauthenticated\b|\bunauthenticated\b|\banonymous\b`)
	versionClaim      = regexp.MustCompile(`(?i)\bv?[0-9]+(?:\.[0-9]+){1,3}(?:[-+][a-z0-9.-]+)?\b|\bv[0-9]+(?:[-+][a-z0-9.-]+)?\b|\bversion\s+[0-9]+(?:\.[0-9]+)*|版本\s*[vV]?[0-9]+(?:\.[0-9]+)*|第[0-9]+版`)
	generalizations   = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:所有|全部|任何|一切)(?:的)?(?:站点|网站|目标|接口|参数|版本|环境|任务|平台|技术栈)|\b(?:all|any|every)\s+(?:sites?|targets?|endpoints?|parameters?|versions?|environments?|tasks?|platforms?)\b|(?:跨平台|跨版本|跨技术栈)(?:通用|适用|可用|有效)`),
		regexp.MustCompile(`(?i)(?:以后|今后|永远|永久|一律).{0,16}(?:不测|不再测|无需|不用|跳过|省略)|(?:无需|不用|不必|不再|跳过|省略).{0,8}(?:验证|测试|复核|检测|扫描|SQLi|SQL[ -]?注入|XSS|鉴权)`),
		regexp.MustCompile(`(?i)\b(?:forever|permanently|always).{0,28}\b(?:skip|omit|stop|avoid|not\s+test)|\b(?:no\s+need\s+to|need\s+not|do\s+not|never)\s+(?:(?:re)?test|verify|validate|check|scan)\b|\b(?:skip|omit|stop)\s+(?:(?:all|future|further)\s+)?(?:SQLi|SQL\s+injection|XSS|tests?|testing|validation|verification|scanning)\b`),
	}
	cautionPrefix = regexp.MustCompile(`(?i)(?:并非|不是|不代表|不意味着|并不适用于|不适用于|未覆盖|尚未覆盖|尚未验证|不应(?:认为|推断)?|不能(?:认为|推断)?|不要|不应|不得|勿|never\s+assume|not\s+applicable\s+to|not\s+for|not\s+to|rather\s+than|do\s+not|must\s+not|never|not)\s*$`)
)

// unsafeGeneralization only catches common, explicit overgeneralizations. It is
// deliberately not a claim that regular expressions prove a note's semantics.
func unsafeGeneralization(text string) bool {
	text = inspectEscapes(cleanInvisible(text))
	for _, pattern := range generalizations {
		for _, location := range pattern.FindAllStringIndex(text, -1) {
			prefix := text[:location[0]]
			if len(prefix) > 96 {
				prefix = prefix[len(prefix)-96:]
			}
			if !cautionPrefix.MatchString(prefix) {
				return true
			}
		}
	}
	return false
}

func containsTargetDetails(fields []string, facts []StageFact) bool {
	identifiers, parameters := []string{}, []string{}
	for _, fact := range facts {
		for _, raw := range append(append([]string(nil), fact.Targets...), fact.Target) {
			target := strings.TrimSpace(inspectEscapes(raw))
			if target == "" {
				continue
			}
			identifiers = append(identifiers, target)
			parsed, err := url.Parse(target)
			if err != nil {
				continue
			}
			if parsed.Hostname() == "" && !strings.HasPrefix(target, "/") {
				parsed, _ = url.Parse("//" + target)
			}
			if parsed == nil {
				continue
			}
			if host := strings.TrimPrefix(parsed.Hostname(), "*."); host != "" {
				identifiers = append(identifiers, strings.TrimSuffix(host, "."))
			}
			if path := parsed.Path; len(path) > 1 {
				identifiers = append(identifiers, path)
			}
			for key := range parsed.Query() {
				if key != "" && len(key) <= 64 {
					parameters = append(parameters, key)
				}
			}
		}
	}
	for _, field := range fields {
		text := inspectEscapes(cleanInvisible(field))
		if sharedURL.MatchString(text) {
			return true
		}
		for _, identifier := range identifiers {
			// Use boundaries for short names so a parameter 'id' does not match
			// unrelated words such as 'validation'. Native target names stay exact.
			if len(identifier) <= 3 {
				pattern := regexp.MustCompile(`(?i)(?:^|[^a-z0-9_])` + regexp.QuoteMeta(identifier) + `(?:$|[^a-z0-9_])`)
				if pattern.MatchString(text) {
					return true
				}
			} else if strings.Contains(strings.ToLower(text), strings.ToLower(identifier)) {
				return true
			}
		}
		for _, key := range parameters {
			quoted := regexp.QuoteMeta(key)
			pattern := regexp.MustCompile(`(?i)(?:[?&]|参数\s*|param(?:eter)?\s*[:=]?\s*)` + quoted + `(?:\s*[=:]|\b)|(?:^|[^a-z0-9_])` + quoted + `\s*(?:参数|parameter\b)`)
			if pattern.MatchString(text) || strings.EqualFold(strings.TrimSpace(text), key) {
				return true
			}
		}
	}
	return false
}

func unsupportedTechnicalClaims(fields, evidence []string) bool {
	platforms, versions, access := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, text := range evidence {
		for _, claim := range platformClaim.FindAllString(text, -1) {
			platforms[canonicalPlatform(claim)] = true
		}
		for _, claim := range versionClaim.FindAllString(text, -1) {
			versions[canonicalVersion(claim)] = true
		}
		for _, claim := range accessClaim.FindAllString(text, -1) {
			access[canonicalAccess(claim)] = true
		}
	}
	for _, text := range fields {
		for _, claim := range platformClaim.FindAllString(text, -1) {
			if !platforms[canonicalPlatform(claim)] {
				return true
			}
		}
		for _, claim := range versionClaim.FindAllString(text, -1) {
			if !versions[canonicalVersion(claim)] {
				return true
			}
		}
		for _, claim := range accessClaim.FindAllString(text, -1) {
			if !access[canonicalAccess(claim)] {
				return true
			}
		}
	}
	return false
}

func canonicalAccess(text string) string {
	switch strings.ToLower(text) {
	case "管理员", "administrator", "admin":
		return "administrator"
	case "超级用户", "superuser":
		return "superuser"
	case "已登录", "已认证", "需要登录", "必须登录", "登录后", "登录状态", "登录会话", "有效会话", "authenticated":
		return "authenticated"
	case "未登录", "无需登录", "匿名", "unauthenticated", "anonymous":
		return "unauthenticated"
	default:
		return strings.ToLower(text)
	}
}

func canonicalPlatform(text string) string {
	switch strings.ToLower(text) {
	case "darwin", "macos":
		return "macos"
	case "aarch64", "arm64":
		return "arm64"
	case "amd64", "x86_64":
		return "amd64"
	default:
		return strings.ToLower(text)
	}
}

func canonicalVersion(text string) string {
	text = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(text), "version"))
	text = strings.TrimSpace(strings.TrimPrefix(text, "版本"))
	return strings.TrimPrefix(text, "v")
}

func cleanInvisible(text string) string {
	return strings.Map(func(r rune) rune {
		if (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') || unicode.In(r, unicode.Cf) || r == '\u034f' {
			return -1
		}
		return r
	}, text)
}

// SanitizeSource removes invisible formatting controls from a COPY used for
// extraction. The source's original bytes/fingerprint must remain unchanged in
// its store. Obvious secret material and pseudo-internal instructions reject
// the entire source; they are not partially redacted or sent to the model.
func SanitizeSource(content string) (string, error) {
	if !utf8.ValidString(content) || len(content) > MaxInputBytes {
		return "", fmt.Errorf("%w: source encoding or size", ErrInvalid)
	}
	clean := cleanInvisible(content)
	// Inspect common escaped representations as well, without changing the
	// content returned or using this form for identity/deduplication.
	inspection := inspectEscapes(clean)
	if containsSensitive(clean) || containsSensitive(inspection) {
		return "", ErrSensitive
	}
	if internalMarker.MatchString(clean) || internalMarker.MatchString(inspection) || authorityChange.MatchString(clean) || authorityChange.MatchString(inspection) {
		return "", fmt.Errorf("%w: instruction-like content", ErrInvalid)
	}
	return clean, nil
}

func inspectEscapes(text string) string {
	for range 2 {
		text = html.UnescapeString(text)
		text = escapedCharacter.ReplaceAllStringFunc(text, func(value string) string {
			decoded, err := strconv.Unquote(`"` + value + `"`)
			if err != nil {
				return value
			}
			return decoded
		})
		text = cleanInvisible(text)
	}
	return text
}

// ValidateContent checks a manually supplied title and body with the same
// conservative secret/instruction filter used for generated notes. It does not
// establish truth, grant permission or prove the absence of all sensitive data.
func ValidateContent(title, content string) error {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(content) == "" || !validField(title, 240) || !validField(content, 4_800) {
		return fmt.Errorf("%w: title or content limits", ErrInvalid)
	}
	if err := validateStableText(title); err != nil {
		return err
	}
	return validateStableText(content)
}

func validateStableText(text string) error {
	if strings.ContainsRune(text, utf8.RuneError) {
		return fmt.Errorf("%w: malformed Unicode text", ErrInvalid)
	}
	clean, err := SanitizeSource(text)
	if err != nil {
		return err
	}
	// Generated/manual notes should never retain an invisible marker or silently
	// change identity during validation. The source sanitizer is intentionally
	// separate because it returns its cleaned COPY explicitly.
	if clean != text {
		return fmt.Errorf("%w: invisible formatting", ErrInvalid)
	}
	return nil
}

func containsSensitive(text string) bool {
	for _, pattern := range sensitivePatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	for _, match := range sensitiveAssignment.FindAllStringSubmatch(text, -1) {
		if !placeholder(match[1]) {
			return true
		}
	}
	for _, match := range bearerValue.FindAllStringSubmatch(text, -1) {
		if !placeholder(match[1]) {
			return true
		}
	}
	for _, match := range basicValue.FindAllStringSubmatch(text, -1) {
		decoded, err := base64.StdEncoding.DecodeString(match[1])
		if err == nil && strings.Contains(string(decoded), ":") && !placeholder(match[1]) {
			return true
		}
	}
	return false
}

func placeholder(value string) bool {
	wrapped := strings.HasPrefix(value, "[") || strings.HasPrefix(value, "<")
	value = strings.Trim(value, "[]<>\"'`")
	if value != "" && strings.Trim(value, "*•…") == "" {
		return true
	}
	if wrapped {
		switch strings.ToUpper(value) {
		case "REDACTED", "REMOVED", "MASKED":
			return true
		}
	}
	switch value {
	case "TOKEN", "API_KEY", "ENCRYPT_KEY", "PASSWORD", "COOKIE", "SECRET", "SERIAL":
		return true
	default:
		return false
	}
}
