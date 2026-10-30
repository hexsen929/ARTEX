package memory

import (
	"encoding/base64"
	"fmt"
	"html"
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
	authorityChange     = regexp.MustCompile(`(?is)(?:(?:ignore|override|bypass|disable|remove|expand|extend)\b.{0,64}\b(?:instructions?|rules?|authorization|scope|permissions?|approval|guard|safeguards?)|(?:忽略|覆盖|绕过|关闭|取消|扩大|扩展).{0,32}(?:指令|授权|范围|审批|规则|权限|安全限制)|(?:用户|管理员)(?:总是|要求|偏好|允许|授权|身份)|\b(?:user|operator|admin)\s+(?:always|prefers?|requires?|allows?|authorizes?)\b|(?:^|\n)\s*(?:you\s+are\b|your\s+(?:role|identity)\b|你(?:现在)?是|你的(?:身份|角色)))`)
	confirmationClaim   = regexp.MustCompile(`(?i)(?:\b(?:confirmed|verified|proven)\s+(?:vulnerability|finding)\b|已确认漏洞|已验证漏洞|确认存在漏洞)`)
	escapedCharacter    = regexp.MustCompile(`\\(?:u[0-9a-fA-F]{4}|x[0-9a-fA-F]{2})`)
)

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
