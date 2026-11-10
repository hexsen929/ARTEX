package report

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestDOCXUsesSavedEvidenceAndValidOOXML(t *testing.T) {
	f := &db.DBFinding{ID: 123, Name: "权限核对 <fixture>", Severity: "high", VerificationStatus: "unverified", EvidenceVersion: 4, ReportEvidenceVersion: 3, Summary: "合成内容，不执行目标请求", Evidence: "GET /fixture\nHTTP/1.1 200", Report: "## 结论\n需要人工复核。", Reproduction: &db.Reproduction{EntryPoint: "fixture.invalid", Prerequisites: []string{"本地合成环境"}, Steps: []db.ReproductionStep{{Title: "核对响应", Request: "GET /fixture", Response: "synthetic response", Expected: "只读测试", Proves: "无真实漏洞结论"}}}}
	for _, locale := range []string{"zh-CN", "zh-TW", "en-US"} {
		t.Run(locale, func(t *testing.T) {
			var out bytes.Buffer
			template := Template{Name: "fixture", Title: "安全评估报告", Company: "Fixture & Co", Client: "Example", Project: "Synthetic only", Locale: locale, Footer: "Confidential fixture"}
			if err := WriteDOCX(&out, template, []DocxFinding{{Finding: f}}, time.Unix(0, 0)); err != nil {
				t.Fatal(err)
			}
			z, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, file := range z.File {
				r, err := file.Open()
				if err != nil {
					t.Fatal(err)
				}
				raw, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(file.Name, ".xml") || strings.HasSuffix(file.Name, ".rels") {
					decoder := xml.NewDecoder(bytes.NewReader(raw))
					for {
						_, err := decoder.Token()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(file.Name, err)
						}
					}
				}
				if file.Name == "word/document.xml" {
					found = true
					for _, want := range []string{"synthetic response", "unverified", "Fixture &amp; Co", "TOC", "权限核对 &lt;fixture&gt;"} {
						if !bytes.Contains(raw, []byte(want)) {
							t.Fatal("missing saved content", want)
						}
					}
				}
				if bytes.Contains(raw, []byte(`TargetMode="External"`)) {
					t.Fatal("external resource in report")
				}
			}
			if !found {
				t.Fatal("missing main document")
			}
			if dir := os.Getenv("ARTEX_DOCX_FIXTURE_DIR"); dir != "" {
				if os.Getenv("CI") == "" {
					t.Fatal("render fixture is cloud only")
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "fixture-"+locale+".docx"), out.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
