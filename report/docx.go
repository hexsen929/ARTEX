package report

// Deterministic OOXML export: no Python installation, macros, external image
// fetching or model generation is involved in rendering a saved report.
import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

type Template struct {
	ID        int64  `json:"id"`
	Version   int64  `json:"version"`
	Name      string `json:"name"`
	Title     string `json:"title"`
	Company   string `json:"company"`
	Client    string `json:"client"`
	Project   string `json:"project"`
	Personnel string `json:"personnel"`
	Locale    string `json:"locale"`
	Footer    string `json:"footer"`
	Logo      string `json:"logo"` // base64 PNG/JPEG bytes, never a path or URL
}
type ReportImage struct {
	Data    []byte
	Caption string
}
type DocxFinding struct {
	Finding *db.DBFinding
	Images  []ReportImage
}

func (t Template) Validate() error {
	if strings.TrimSpace(t.Name) == "" || len(t.Name) > 120 || len(t.Title) > 240 || len(t.Company) > 240 || len(t.Client) > 240 || len(t.Project) > 240 || len(t.Personnel) > 2000 || len(t.Footer) > 500 {
		return errors.New("报告模板字段过长或名称为空")
	}
	if t.Locale != "zh-CN" && t.Locale != "zh-TW" && t.Locale != "en-US" {
		return errors.New("不支持的报告语言")
	}
	for _, s := range []string{t.Name, t.Title, t.Company, t.Client, t.Project, t.Personnel, t.Footer} {
		if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
			return errors.New("报告模板文字无效")
		}
	}
	if t.Logo != "" {
		if len(t.Logo) > 360000 {
			return errors.New("logo 超过 256 KiB")
		}
		raw, err := base64.StdEncoding.DecodeString(t.Logo)
		if err != nil || len(raw) > 256<<10 {
			return errors.New("logo 编码无效")
		}
		if _, _, _, err = pictureInfo(raw); err != nil {
			return err
		}
	}
	return nil
}
func pictureInfo(raw []byte) (int, int, string, error) {
	if len(raw) > 3<<20 {
		return 0, 0, "", errors.New("报告图片过大")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 8000 || cfg.Height > 8000 {
		return 0, 0, "", errors.New("报告图片必须是有效 PNG/JPEG")
	}
	return cfg.Width, cfg.Height, format, nil
}
func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

type wordDocument struct {
	body    strings.Builder
	images  []ReportImage
	formats []string
}

func (d *wordDocument) p(style, text string) {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		d.body.WriteString(`<w:p><w:pPr><w:pStyle w:val="` + style + `"/></w:pPr><w:r><w:t xml:space="preserve">` + xmlText(line) + `</w:t></w:r></w:p>`)
	}
}
func (d *wordDocument) table(rows [][2]string) {
	d.body.WriteString(`<w:tbl><w:tblPr><w:tblW w:w="0" w:type="auto"/><w:tblBorders><w:top w:val="single" w:sz="4" w:color="D5D9DF"/><w:bottom w:val="single" w:sz="4" w:color="D5D9DF"/><w:insideH w:val="single" w:sz="4" w:color="D5D9DF"/></w:tblBorders></w:tblPr><w:tblGrid><w:gridCol w:w="2400"/><w:gridCol w:w="6600"/></w:tblGrid>`)
	for _, row := range rows {
		d.body.WriteString(`<w:tr>`)
		for _, s := range row {
			d.body.WriteString(`<w:tc><w:tcPr><w:tcW w:w="4500" w:type="dxa"/></w:tcPr>`)
			d.p("Normal", s)
			d.body.WriteString(`</w:tc>`)
		}
		d.body.WriteString(`</w:tr>`)
	}
	d.body.WriteString(`</w:tbl>`)
}
func (d *wordDocument) picture(img ReportImage) error {
	w, h, format, err := pictureInfo(img.Data)
	if err != nil {
		return err
	}
	n := len(d.images) + 1
	d.images = append(d.images, img)
	d.formats = append(d.formats, format)
	cx := int64(5200000)
	cy := cx * int64(h) / int64(w)
	if cy > 6200000 {
		cx = cx * 6200000 / cy
		cy = 6200000
	}
	d.body.WriteString(fmt.Sprintf(`<w:p><w:r><w:drawing><wp:inline distT="0" distB="0" distL="0" distR="0"><wp:extent cx="%d" cy="%d"/><wp:docPr id="%d" name="Evidence %d"/><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic><pic:nvPicPr><pic:cNvPr id="%d" name="Evidence %d"/><pic:cNvPicPr/></pic:nvPicPr><pic:blipFill><a:blip r:embed="img%d"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill><pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%d" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>`, cx, cy, n, n, n, n, n, cx, cy))
	if img.Caption != "" {
		d.p("Caption", img.Caption)
	}
	return nil
}
func (d *wordDocument) markdown(s string) {
	code := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			code = !code
			continue
		}
		if code {
			d.p("Code", line)
		} else if strings.HasPrefix(line, "#") {
			d.p("Heading2", strings.TrimSpace(strings.TrimLeft(line, "#")))
		} else {
			d.p("Normal", line)
		}
	}
}

func WriteDOCX(w io.Writer, t Template, findings []DocxFinding, now time.Time) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if len(findings) > 100 {
		return errors.New("单份报告最多 100 个漏洞")
	}
	loc := func(cn, tw, en string) string {
		if t.Locale == "en-US" {
			return en
		}
		if t.Locale == "zh-TW" {
			return tw
		}
		return cn
	}
	d := &wordDocument{}
	if t.Logo != "" {
		raw, _ := base64.StdEncoding.DecodeString(t.Logo)
		if err := d.picture(ReportImage{Data: raw}); err != nil {
			return err
		}
	}
	title := t.Title
	if title == "" {
		title = loc("安全测试报告", "安全測試報告", "Security assessment report")
	}
	d.p("Title", title)
	d.p("Subtitle", t.Project)
	d.table([][2]string{{loc("出具方", "出具方", "Prepared by"), t.Company}, {loc("客户", "客戶", "Client"), t.Client}, {loc("测试人员", "測試人員", "Personnel"), t.Personnel}, {loc("生成时间", "生成時間", "Generated"), now.Format(time.RFC3339)}})
	d.p("Normal", loc("本报告复用已保存的结果，不代表新增测试。模板语言只改变固定标题，不自动翻译证据或报告原文。", "本報告重用已儲存結果，不代表新增測試。範本語言只改變固定標題，不翻譯證據或原文。", "This report uses saved results; no new tests were performed. Template language changes labels only, not original evidence or narrative."))
	d.body.WriteString(`<w:p><w:r><w:br w:type="page"/></w:r></w:p>`)
	d.p("Heading1", loc("目录", "目錄", "Contents"))
	d.body.WriteString(`<w:p><w:fldSimple w:instr='TOC \o "1-2" \h \z \u'><w:r><w:t>` + xmlText(loc("在 Word 中更新目录域", "在 Word 中更新目錄欄位", "Update this table of contents in Word")) + `</w:t></w:r></w:fldSimple></w:p>`)
	d.p("Heading1", loc("结果摘要", "結果摘要", "Summary"))
	counts := map[string]int{}
	for _, item := range findings {
		counts[item.Finding.VerificationStatus]++
	}
	d.table([][2]string{{loc("记录总数", "記錄總數", "Records"), fmt.Sprint(len(findings))}, {loc("已验证", "已驗證", "Confirmed"), fmt.Sprint(counts["confirmed"])}, {loc("其他状态", "其他狀態", "Other verification states"), fmt.Sprint(len(findings) - counts["confirmed"])}})
	for i, item := range findings {
		f := item.Finding
		d.p("Heading1", fmt.Sprintf("%d. %s", i+1, findingTitle(f)))
		d.table([][2]string{{"ID", fmt.Sprint(f.ID)}, {loc("严重程度", "嚴重程度", "Severity"), f.Severity}, {loc("验证状态", "驗證狀態", "Verification"), f.VerificationStatus}, {loc("验证依据", "驗證依據", "Verification basis"), f.VerificationNote}, {loc("处理状态", "處理狀態", "Triage status"), f.Status}, {loc("证据版本", "證據版本", "Evidence version"), fmt.Sprint(f.EvidenceVersion)}})
		d.p("Normal", f.Summary)
		if f.Reproduction != nil {
			r := f.Reproduction
			d.p("Heading2", loc("复现过程", "重現過程", "Reproduction"))
			d.p("Normal", r.EntryPoint)
			for _, pre := range r.Prerequisites {
				d.p("Normal", "• "+pre)
			}
			for j, step := range r.Steps {
				d.p("Heading2", fmt.Sprintf("%s %d: %s", loc("步骤", "步驟", "Step"), j+1, step.Title))
				d.p("Code", step.Request)
				d.p("Code", step.Response)
				d.p("Normal", loc("预期：", "預期：", "Expected: ")+step.Expected)
				d.p("Normal", loc("证明与局限：", "證明與限制：", "Observed proof and limits: ")+step.Proves)
				if len(step.TrafficIDs) > 0 {
					d.p("Caption", "Traffic IDs: "+strings.Join(step.TrafficIDs, ", "))
				}
			}
			d.p("Caption", r.Provenance)
		}
		if f.Report != "" {
			d.p("Heading2", loc("保存的报告", "儲存的報告", "Saved narrative"))
			if f.ReportEvidenceVersion != f.EvidenceVersion {
				d.p("Normal", loc("报告对应旧证据版本，请复核。", "報告對應舊證據版本，請覆核。", "Narrative refers to an older evidence version; review required."))
			}
			d.markdown(f.Report)
		}
		if f.Evidence != "" {
			d.p("Heading2", loc("原始证据", "原始證據", "Original evidence"))
			d.p("Code", f.Evidence)
		}
		for _, image := range item.Images {
			if err := d.picture(image); err != nil {
				return err
			}
		}
		for _, b := range f.TrafficBindings {
			d.p("Caption", fmt.Sprintf("Traffic ID: %s · snapshot: %s · %s", b.Snapshot.SourceTrafficID, b.SnapshotID, b.Role))
		}
	}
	if d.body.Len() > 8<<20 {
		return errors.New("报告文字超过 8 MiB，请减少选择")
	}
	const relNS = "http://schemas.openxmlformats.org/package/2006/relationships"
	const officeRel = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"
	document := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture"><w:body>` + d.body.String() + `<w:sectPr><w:footerReference w:type="default" r:id="footer"/><w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1134" w:right="1134" w:bottom="1134" w:left="1134" w:header="567" w:footer="567"/></w:sectPr></w:body></w:document>`
	rels := `<Relationships xmlns="` + relNS + `"><Relationship Id="styles" Type="` + officeRel + `styles" Target="styles.xml"/><Relationship Id="settings" Type="` + officeRel + `settings" Target="settings.xml"/><Relationship Id="footer" Type="` + officeRel + `footer" Target="footer1.xml"/>`
	files := map[string][]byte{"word/document.xml": []byte(document), "word/styles.xml": []byte(wordStyles), "word/settings.xml": []byte(`<w:settings xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:updateFields w:val="true"/></w:settings>`), "_rels/.rels": []byte(`<Relationships xmlns="` + relNS + `"><Relationship Id="document" Type="` + officeRel + `officeDocument" Target="word/document.xml"/></Relationships>`)}
	for i, img := range d.images {
		ext := d.formats[i]
		name := fmt.Sprintf("image%d.%s", i+1, ext)
		files["word/media/"+name] = img.Data
		rels += fmt.Sprintf(`<Relationship Id="img%d" Type="%simage" Target="media/%s"/>`, i+1, officeRel, name)
	}
	files["word/_rels/document.xml.rels"] = []byte(rels + `</Relationships>`)
	files["word/footer1.xml"] = []byte(`<w:ftr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:p><w:pPr><w:jc w:val="center"/></w:pPr><w:r><w:t>` + xmlText(t.Footer) + ` · </w:t></w:r><w:fldSimple w:instr="PAGE"/><w:r><w:t> / </w:t></w:r><w:fldSimple w:instr="NUMPAGES"/></w:p></w:ftr>`)
	files["[Content_Types].xml"] = []byte(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Default Extension="png" ContentType="image/png"/><Default Extension="jpeg" ContentType="image/jpeg"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/><Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/><Override PartName="/word/settings.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.settings+xml"/><Override PartName="/word/footer1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.footer+xml"/></Types>`)
	z := zip.NewWriter(w)
	for name, raw := range files {
		f, err := z.Create(name)
		if err != nil {
			return err
		}
		if _, err = f.Write(raw); err != nil {
			return err
		}
	}
	return z.Close()
}

const wordStyles = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Calibri" w:hAnsi="Calibri" w:eastAsia="Noto Sans CJK SC"/><w:sz w:val="22"/></w:rPr></w:rPrDefault><w:pPrDefault><w:pPr><w:spacing w:after="120" w:line="280" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults><w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style><w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/><w:rPr><w:b/><w:sz w:val="40"/><w:color w:val="24435B"/></w:rPr></w:style><w:style w:type="paragraph" w:styleId="Subtitle"><w:name w:val="Subtitle"/><w:rPr><w:sz w:val="28"/></w:rPr></w:style><w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:pPr><w:keepNext/><w:outlineLvl w:val="0"/><w:spacing w:before="300" w:after="180"/></w:pPr><w:rPr><w:b/><w:sz w:val="30"/><w:color w:val="24435B"/></w:rPr></w:style><w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:pPr><w:keepNext/><w:outlineLvl w:val="1"/><w:spacing w:before="180" w:after="100"/></w:pPr><w:rPr><w:b/><w:sz w:val="24"/></w:rPr></w:style><w:style w:type="paragraph" w:styleId="Code"><w:name w:val="Code"/><w:pPr><w:shd w:fill="F3F5F7"/><w:wordWrap/></w:pPr><w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:eastAsia="Noto Sans Mono CJK SC"/><w:sz w:val="18"/></w:rPr></w:style><w:style w:type="paragraph" w:styleId="Caption"><w:name w:val="Caption"/><w:rPr><w:color w:val="666666"/><w:sz w:val="18"/></w:rPr></w:style></w:styles>`
