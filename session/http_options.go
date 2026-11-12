package session

import (
	"context"
	"fmt"
	"golang.org/x/text/encoding/simplifiedchinese"
	"strings"
	"time"
	"unicode/utf8"
)

type HTTPOptions struct {
	Method       string `json:"method,omitempty"`
	CommandParam string `json:"cmd_param,omitempty"`
	Encoding     string `json:"encoding,omitempty"`
	TargetOS     string `json:"target_os,omitempty"`
}

func (o HTTPOptions) Validate() error {
	m := strings.ToUpper(o.Method)
	if m != "" && m != "GET" && m != "POST" {
		return fmt.Errorf("method must be GET or POST")
	}
	switch strings.ToLower(o.Encoding) {
	case "", "auto", "utf-8", "utf8", "gbk", "gb18030":
	default:
		return fmt.Errorf("unsupported response encoding")
	}
	if len(o.CommandParam) > 200 || strings.ContainsAny(o.CommandParam, "\x00\r\n") {
		return fmt.Errorf("invalid command parameter")
	}
	return nil
}
func (s *HTTPShell) applyHTTPOptions(o HTTPOptions) error {
	if err := o.Validate(); err != nil {
		return err
	}
	s.httpOptions = o
	if o.CommandParam != "" {
		s.password = o.CommandParam
	}
	if o.TargetOS != "" {
		s.SetPlatform(o.TargetOS)
	}
	return nil
}
func decodeShellBody(b []byte, encoding string) (string, error) {
	switch strings.ToLower(encoding) {
	case "gbk", "gb18030":
		v, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b)
		return string(v), err
	case "auto":
		if !utf8.Valid(b) {
			v, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b)
			return string(v), err
		}
	}
	return string(b), nil
}
func (s *HTTPShell) Eval(ctx context.Context, code string, timeout time.Duration) (string, error) {
	if s.lang != "php" || s.enc != nil {
		return "", fmt.Errorf("raw eval requires a plaintext PHP eval session")
	}
	if len(code) > 128<<10 {
		return "", fmt.Errorf("code too large")
	}
	s.execMu.Lock()
	defer s.execMu.Unlock()
	m := newSentinel()
	return s.phpFileOp(ctx, "echo '"+m.start+"';"+code+";echo '"+m.end+"';", m, timeout)
}
