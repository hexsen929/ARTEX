package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/report"
)

func (s *Server) registerReportTemplateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/report-templates", s.listReportTemplates)
	mux.HandleFunc("POST /api/report-templates", s.saveReportTemplate)
	mux.HandleFunc("PUT /api/report-templates/{id}", s.saveReportTemplate)
	mux.HandleFunc("DELETE /api/report-templates/{id}", s.deleteReportTemplate)
	mux.HandleFunc("POST /api/reports/docx", s.exportReportDOCX)
}
func (s *Server) listReportTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := s.m.pg.ReportTemplates(r.Context())
	if err != nil {
		writeErr(w, 503, "模板读取失败")
		return
	}
	out := []report.Template{}
	for _, row := range rows {
		var t report.Template
		if json.Unmarshal(row.Body, &t) != nil {
			writeErr(w, 503, "模板格式无效")
			return
		}
		t.ID, t.Version = row.ID, row.Version
		out = append(out, t)
	}
	writeJSON(w, 200, map[string]any{"items": out})
}
func (s *Server) saveReportTemplate(w http.ResponseWriter, r *http.Request) {
	var t report.Template
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 400000))
	d.DisallowUnknownFields()
	if d.Decode(&t) != nil || d.Decode(new(any)) != io.EOF {
		writeErr(w, 400, "模板格式无效或过大")
		return
	}
	if r.Method == "PUT" {
		id, ok := pathInt(r, "id")
		if !ok || id <= 0 || (t.ID != 0 && t.ID != id) {
			writeErr(w, 400, "模板 ID 无效")
			return
		}
		t.ID = id
	} else if t.ID != 0 {
		writeErr(w, 400, "新模板不能指定 ID")
		return
	}
	if err := t.Validate(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	raw, _ := json.Marshal(t)
	row, err := s.m.pg.SaveReportTemplate(r.Context(), db.ReportTemplateRow{ID: t.ID, Version: t.Version, Body: raw})
	if err != nil {
		writeErr(w, 409, "模板已变化或数量超限，请刷新核对")
		return
	}
	t.ID, t.Version = row.ID, row.Version
	writeJSON(w, 200, t)
}
func (s *Server) deleteReportTemplate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	version, err := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
	if !ok || id < 1 || err != nil || version < 1 {
		writeErr(w, 400, "模板 ID 或版本无效")
		return
	}
	if err = s.m.pg.DeleteReportTemplate(r.Context(), id, version); err != nil {
		writeErr(w, 409, "模板已变化，请刷新")
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}
func (s *Server) exportReportDOCX(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TemplateID        int64 `json:"template_id"`
		TemplateVersion   int64 `json:"template_version"`
		IncludeUnverified bool  `json:"include_unverified"`
		Findings          []struct {
			ID              int64 `json:"id"`
			EvidenceVersion int64 `json:"evidence_version"`
		} `json:"findings"`
	}
	if !knowledgeDecode(w, r, &in) {
		return
	}
	if len(in.Findings) == 0 || len(in.Findings) > 100 {
		writeErr(w, 400, "请选择 1–100 条漏洞记录")
		return
	}
	rows, err := s.m.pg.ReportTemplates(r.Context())
	if err != nil {
		writeErr(w, 503, "模板读取失败")
		return
	}
	var template report.Template
	found := false
	for _, row := range rows {
		if row.ID == in.TemplateID && row.Version == in.TemplateVersion {
			if json.Unmarshal(row.Body, &template) == nil {
				found = true
			}
			break
		}
	}
	if !found {
		writeErr(w, 409, "模板已变化，请刷新选择")
		return
	}
	items := []report.DocxFinding{}
	seen := map[int64]bool{}
	textBytes, imageBytes, imageCount := 0, 0, 0
	for _, ref := range in.Findings {
		if ref.ID <= 0 || seen[ref.ID] {
			writeErr(w, 400, "漏洞 ID 无效或重复")
			return
		}
		seen[ref.ID] = true
		f, err := s.m.pg.GetFinding(ref.ID)
		if err != nil || f == nil {
			writeErr(w, 404, "所选漏洞不存在")
			return
		}
		if f.EvidenceVersion != ref.EvidenceVersion {
			writeErr(w, 409, "漏洞证据已变化，请刷新后生成")
			return
		}
		if !in.IncludeUnverified && f.VerificationStatus != db.VerificationConfirmed {
			writeErr(w, 409, "默认仅导出已验证漏洞；请重新选择或明确包含其他状态")
			return
		}
		flow, err := s.m.pg.GetFindingTraffic(r.Context(), f.ID)
		if err != nil {
			writeErr(w, 503, "证据读取失败")
			return
		}
		if flow.Version != f.EvidenceVersion {
			writeErr(w, 409, "证据已变化")
			return
		}
		f.TrafficBindings = flow.Bindings
		item := report.DocxFinding{Finding: f}
		textBytes += len(f.Report) + len(f.Evidence) + len(f.Summary)
		if f.Reproduction != nil {
			raw, _ := json.Marshal(f.Reproduction)
			textBytes += len(raw)
		}
		if textBytes > 2<<20 {
			writeErr(w, 413, "报告文字过多，请减少选择")
			return
		}
		for _, match := range screenshotMarkdownLink.FindAllStringSubmatch(f.Report, -1) {
			if imageCount >= 20 {
				writeErr(w, 413, "单份报告最多 20 张截图")
				return
			}
			file, err := s.openPortableReportScreenshot(match[2], f.ID)
			if err != nil {
				continue
			}
			raw, e := io.ReadAll(io.LimitReader(file, 3<<20+1))
			file.Close()
			if e != nil || len(raw) > 3<<20 {
				writeErr(w, 413, "证据截图过大或不可读")
				return
			}
			imageBytes += len(raw)
			if imageBytes > 32<<20 {
				writeErr(w, 413, "报告图片总量过大")
				return
			}
			item.Images = append(item.Images, report.ReportImage{Data: raw, Caption: match[1]})
			imageCount++
		}
		items = append(items, item)
	}
	var output bytes.Buffer
	if err := report.WriteDOCX(&output, template, items, time.Now()); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Do not hand out a report whose evidence was edited while it was rendered.
	for _, item := range items {
		current, e := s.m.pg.GetFinding(item.Finding.ID)
		if e != nil || current == nil || current.EvidenceVersion != item.Finding.EvidenceVersion || current.VerificationStatus != item.Finding.VerificationStatus {
			writeErr(w, 409, "生成期间漏洞已变化，请重新生成")
			return
		}
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	w.Header().Set("Content-Disposition", `attachment; filename="security-report.docx"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(output.Bytes())
}
