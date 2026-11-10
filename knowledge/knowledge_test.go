package knowledge

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractionAndChunkBounds(t *testing.T) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, _ := z.Create("word/document.xml")
	f.Write([]byte(`<w:document xmlns:w="fixture"><w:p><w:r><w:t>接口权限 &amp; evidence</w:t></w:r></w:p></w:document>`))
	z.Close()
	text, err := Extract(t.Context(), "guide.docx", b.Bytes())
	if err != nil || text != "接口权限 & evidence\n" {
		t.Fatalf("extraction %q %v", text, err)
	}
	parts, err := Chunk(strings.Repeat("字", 3000))
	if err != nil || len(parts) != 4 {
		t.Fatal(len(parts), err)
	}
	for _, p := range parts {
		if len([]rune(p)) > 1000 {
			t.Fatal("oversize chunk")
		}
	}
	if _, err = Extract(t.Context(), "guide.txt", bytes.Repeat([]byte("A"), MaxText+1)); err == nil {
		t.Fatal("large text accepted")
	}
	if _, err = Extract(t.Context(), "guide.txt", []byte{'a', 0, 'b'}); err == nil {
		t.Fatal("NUL accepted")
	}
	if _, err = Chunk(strings.Repeat("a", MaxText)); err != ErrQuota {
		t.Fatal("chunk budget ignored", err)
	}
}

func TestEmbedValidatesResponseAndNeverFollowsRedirect(t *testing.T) {
	mode := "ok"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/embeddings" {
			t.Error("wrong endpoint")
			http.Error(w, "bad", 400)
			return
		}
		if mode == "redirect" {
			w.Header().Set("Location", "/other")
			w.WriteHeader(302)
			return
		}
		if mode == "duplicate" {
			io := map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float32{1, 0}}, map[string]any{"index": 0, "embedding": []float32{0, 1}}}}
			json.NewEncoder(w).Encode(io)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 1, "embedding": []float32{0, 1}}, map[string]any{"index": 0, "embedding": []float32{1, 0}}}})
	}))
	defer server.Close()
	c := Config{Enabled: true, BaseURL: server.URL, Model: "fixture", Dimension: 2}
	v, err := Embed(t.Context(), c, []string{"one", "two"})
	if err != nil || v[0][0] != 1 || v[1][1] != 1 {
		t.Fatal(v, err)
	}
	mode = "duplicate"
	if _, err = Embed(t.Context(), c, []string{"one", "two"}); err == nil {
		t.Fatal("duplicate indices accepted")
	}
	mode = "redirect"
	before := calls
	if _, err = Embed(t.Context(), c, []string{"one", "two"}); err == nil || calls != before+1 {
		t.Fatal("redirect followed or accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = Embed(ctx, c, []string{"one"}); err == nil {
		t.Fatal("cancellation ignored")
	}
}
