// Package knowledge handles bounded reference documents, independently of learned
// experiences. Documents are untrusted data, never task authorization or policy.
package knowledge

import (
	"errors"
	"math"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	MaxText          = 256 << 10
	MaxUpload        = 8 << 20
	MaxChunks        = 128
	MaxLibraryBytes  = 16 << 20
	MaxLibraryChunks = 4096
	MaxDocuments     = 256
)

var ErrInput = errors.New("invalid knowledge input")
var ErrConflict = errors.New("knowledge changed; reload before retrying")
var ErrQuota = errors.New("knowledge library limit reached")

type Document struct {
	ID       int64  `json:"id"`
	TaskID   int64  `json:"task_id"` // zero is explicitly global
	Title    string `json:"title"`
	Category string `json:"category"`
	Content  string `json:"content,omitempty"`
	FileName string `json:"file_name"`
	Version  int64  `json:"version"`
	Bytes    int    `json:"bytes"`
	Chunks   int    `json:"chunks"`
	Indexed  bool   `json:"indexed"`
}

type Config struct {
	Enabled         bool   `json:"enabled"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	Dimension       int    `json:"dimension"`
	APIKey          string `json:"-"`
	HasKey          bool   `json:"has_key"`
	Epoch           int64  `json:"epoch"`
	VectorAvailable bool   `json:"vector_available"`
}

type Hit struct {
	DocID   int64   `json:"doc_id"`
	Version int64   `json:"version"`
	Chunk   int     `json:"chunk"`
	TaskID  int64   `json:"task_id"`
	Title   string  `json:"title"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

type Result struct {
	Mode   string `json:"mode"`
	Notice string `json:"notice"`
	Hits   []Hit  `json:"hits"`
}

func ValidText(s string, maximum int) bool {
	return len(s) <= maximum && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func (d Document) Validate() error {
	if d.TaskID < 0 || strings.TrimSpace(d.Title) == "" || !ValidText(d.Title, 240) || !ValidText(d.Category, 120) || !ValidText(d.Content, MaxText) || strings.TrimSpace(d.Content) == "" || !ValidText(d.FileName, 240) {
		return ErrInput
	}
	return nil
}

func (c Config) Validate() error {
	if c.Dimension < 1 || c.Dimension > 2048 || !ValidText(c.Model, 200) || !ValidText(c.APIKey, 8192) || len(c.BaseURL) > 2048 {
		return ErrInput
	}
	if c.BaseURL == "" && !c.Enabled {
		return nil
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))) {
		return ErrInput
	}
	if c.Enabled && strings.TrimSpace(c.Model) == "" {
		return ErrInput
	}
	return nil
}

// Chunk keeps overlapping Unicode text without duplicating whole documents in
// results. The cap applies to both keyword and vector modes.
func Chunk(text string) ([]string, error) {
	r := []rune(strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")))
	var out []string
	for len(r) > 0 {
		n := min(1000, len(r))
		out = append(out, string(r[:n]))
		if len(out) > MaxChunks {
			return nil, ErrQuota
		}
		if n == len(r) {
			break
		}
		r = r[n-80:]
	}
	return out, nil
}

func ValidVector(v []float32, dimension int) bool {
	if len(v) != dimension {
		return false
	}
	var norm float64
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
		norm += float64(x) * float64(x)
	}
	return norm > 0
}
