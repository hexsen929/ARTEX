package knowledge

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// No original uploads are retained. DOCX extraction never reads relationships or
// external targets; PDF uses a bounded, cancellable local Poppler subprocess.
func Extract(ctx context.Context, name string, raw []byte) (string, error) {
	if len(raw) > MaxUpload {
		return "", ErrQuota
	}
	var text string
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown", ".txt", ".csv", ".json", ".yaml", ".yml":
		text = string(raw)
	case ".docx":
		z, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			return "", ErrInput
		}
		var document *zip.File
		for _, f := range z.File {
			if f.Name == "word/document.xml" {
				if document != nil {
					return "", ErrInput
				}
				document = f
			}
		}
		if document == nil || document.UncompressedSize64 > 4<<20 {
			return "", ErrQuota
		}
		f, err := document.Open()
		if err != nil {
			return "", ErrInput
		}
		defer f.Close()
		decoder := xml.NewDecoder(io.LimitReader(f, 4<<20+1))
		var b strings.Builder
		inText := false
		for {
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", ErrInput
			}
			switch t := token.(type) {
			case xml.StartElement:
				if t.Name.Local == "t" {
					inText = true
				}
			case xml.EndElement:
				if t.Name.Local == "t" {
					inText = false
				}
				if t.Name.Local == "p" {
					b.WriteByte('\n')
				}
			case xml.CharData:
				if inText {
					b.Write(t)
				}
			}
			if b.Len() > MaxText {
				return "", ErrQuota
			}
		}
		text = b.String()
	case ".pdf":
		binary, err := exec.LookPath("pdftotext")
		if err != nil {
			return "", errors.New("PDF 导入需要 Poppler 的 pdftotext；可先转为文本或 DOCX")
		}
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-enc", "UTF-8", "-", "-")
		cmd.Stdin = bytes.NewReader(raw)
		cmd.WaitDelay = time.Second
		output := &boundedOutput{}
		cmd.Stdout = output
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			return "", errors.New("PDF 解析失败或超时")
		}
		if output.overflow {
			return "", ErrQuota
		}
		text = output.String()
	default:
		return "", errors.New("支持 TXT、Markdown、CSV、JSON、YAML、DOCX 和 PDF")
	}
	if !ValidText(text, MaxText) || strings.TrimSpace(text) == "" {
		return "", ErrInput
	}
	return text, nil
}

type boundedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	left := MaxText - b.Len()
	if n > left {
		b.overflow = true
	}
	if left > 0 {
		b.Buffer.Write(p[:min(left, n)])
	}
	return n, nil
}
