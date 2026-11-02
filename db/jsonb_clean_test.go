package db

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestJsonbClean(t *testing.T) {
	// A marshaled payload carrying a NUL byte (e.g. captured HTTP/tool output).
	b, err := json.Marshal(map[string]string{"body": "ab\x00cd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `\u0000`) {
		t.Fatalf("precondition: marshaled JSON should contain the NUL escape, got %s", b)
	}

	cleaned := jsonbClean(b)
	if strings.Contains(string(cleaned), `\u0000`) {
		t.Fatalf("jsonbClean left a NUL escape jsonb rejects: %s", cleaned)
	}

	// Result must stay valid JSON with the NUL simply dropped.
	var out map[string]string
	if err := json.Unmarshal(cleaned, &out); err != nil {
		t.Fatalf("cleaned bytes are not valid JSON: %v (%s)", err, cleaned)
	}
	if out["body"] != "abcd" {
		t.Fatalf("expected NUL stripped to \"abcd\", got %q", out["body"])
	}

	// A NUL escape typed literally in source text (doubled backslash) is preserved.
	lit := []byte(`{"body":"\\u0000"}`)
	if got := jsonbClean(lit); string(got) != string(lit) {
		t.Fatalf("literal \\\\u0000 must be untouched, got %s", got)
	}

	// No NUL escape → returned unchanged.
	plain := []byte(`{"body":"hello"}`)
	if got := jsonbClean(plain); string(got) != string(plain) {
		t.Fatalf("plain JSON must be untouched, got %s", got)
	}
}

func TestJsonbCleanUnicodeEscapesAndOriginalEncoding(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"isolated low surrogate", `{"value":"\udc00"}`, `{"value":"\ufffd"}`},
		{"isolated high surrogate", `{"value":"\uD800"}`, `{"value":"\ufffd"}`},
		{"surrogate boundaries", `{"value":"\uDBFFx\uDFFF"}`, `{"value":"\ufffdx\ufffd"}`},
		{"valid pair keeps original hex case", `{"value":"\uD83d\uDe00"}`, `{"value":"\uD83d\uDe00"}`},
		{"adjacent ordinary scalars", `{"value":"\uD7ff\uE000\ufffd"}`, `{"value":"\uD7ff\uE000\ufffd"}`},
		{"literal escapes stay literal", `{"value":"\\uD800 \\udc00 \\u0000"}`, `{"value":"\\uD800 \\udc00 \\u0000"}`},
		{"literal slash before real surrogate", `{"value":"\\\uDC00"}`, `{"value":"\\\ufffd"}`},
		{"even slash run stays literal", `{"value":"\\\\uDC00"}`, `{"value":"\\\\uDC00"}`},
		{"keys and values", `{"\uDC00":"\uD800","next":"ok"}`, `{"\ufffd":"\ufffd","next":"ok"}`},
		{"NUL does not create a new surrogate pair", `{"value":"\ud800\u0000\udc00"}`, `{"value":"\ufffd\ufffd"}`},
		{"a later original pair stays paired", `{"value":"\ud800\ud800\udc00"}`, `{"value":"\ufffd\ud800\udc00"}`},
		{"a literal following escape cannot pair", `{"value":"\ud800\\udc00"}`, `{"value":"\ufffd\\udc00"}`},
		{"escaped quotes do not end a string", `{"quote":"\"","bad":"\udc00","tail":"\\"}`, `{"quote":"\"","bad":"\ufffd","tail":"\\"}`},
		{"NUL after literal backslash", `{"value":"\\\u0000tail"}`, `{"value":"\\tail"}`},
		{"real UTF8 and legal escapes stay identical", `{"value":"中文😀\n\t\/\u0022"}`, `{"value":"中文😀\n\t\/\u0022"}`},
		{"numbers keys and duplicates are not decoded", `{"z":1793557371911967000,"a":0.123456789012345678901234567890,"exp":1.2300e+40,"neg":-0.0,"dup":1,"dup":2,"bad":"\uDC00"}`, `{"z":1793557371911967000,"a":0.123456789012345678901234567890,"exp":1.2300e+40,"neg":-0.0,"dup":1,"dup":2,"bad":"\ufffd"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.in)
			before := bytes.Clone(input)
			if !json.Valid(input) || !json.Valid([]byte(tc.want)) {
				t.Fatal("fixture must have valid Go JSON syntax")
			}
			got := jsonbClean(input)
			if !bytes.Equal(got, []byte(tc.want)) {
				t.Fatalf("encoding changed unexpectedly: got %s want %s", got, tc.want)
			}
			if !bytes.Equal(input, before) {
				t.Fatal("normalization mutated caller-owned checkpoint bytes")
			}
			if !json.Valid(got) || !bytes.Equal(jsonbClean(got), got) {
				t.Fatal("normalized JSON must remain valid and idempotent")
			}
		})
	}
}
