// Package c2 implements explicit, authenticated HTTP/TCP operator sessions.
package c2

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const MaxFrame = 4 << 20

type Envelope struct {
	Key  []byte
	Mode string
}

func ParseKey(s string) ([]byte, error) {
	b, e := base64.StdEncoding.DecodeString(s)
	if e != nil || len(b) != 32 {
		return nil, fmt.Errorf("32-byte base64 key required")
	}
	return b, nil
}
func derive(key []byte, label string) []byte {
	v := append(append([]byte(nil), key...), []byte(label)...)
	h := sha256.Sum256(v)
	return h[:]
}
func (e Envelope) Seal(raw []byte, mode string) ([]byte, error) {
	if len(e.Key) != 32 || len(raw) > MaxFrame {
		return nil, fmt.Errorf("invalid key or message size")
	}
	if mode == "" || mode == "auto" {
		mode = e.Mode
	}
	if mode == "" || mode == "auto" {
		mode = "cbc-hmac"
	}
	var out []byte
	switch mode {
	case "gcm":
		b, _ := aes.NewCipher(e.Key)
		g, _ := cipher.NewGCM(b)
		n := make([]byte, g.NonceSize())
		if _, err := io.ReadFull(rand.Reader, n); err != nil {
			return nil, err
		}
		out = g.Seal(n, n, raw, nil)
	case "cbc-hmac":
		b, _ := aes.NewCipher(derive(e.Key, "enc"))
		iv := make([]byte, aes.BlockSize)
		if _, err := io.ReadFull(rand.Reader, iv); err != nil {
			return nil, err
		}
		pad := aes.BlockSize - len(raw)%aes.BlockSize
		p := append([]byte(nil), raw...)
		for i := 0; i < pad; i++ {
			p = append(p, byte(pad))
		}
		out = make([]byte, len(iv)+len(p))
		copy(out, iv)
		cipher.NewCBCEncrypter(b, iv).CryptBlocks(out[16:], p)
		h := hmac.New(sha256.New, derive(e.Key, "mac"))
		h.Write(out)
		out = append(out, h.Sum(nil)...)
	default:
		return nil, fmt.Errorf("unsupported envelope")
	}
	return json.Marshal(map[string]string{"d": base64.StdEncoding.EncodeToString(out)})
}
func (e Envelope) Open(wire []byte) ([]byte, string, error) {
	if len(e.Key) != 32 || len(wire) > MaxFrame {
		return nil, "", fmt.Errorf("invalid envelope")
	}
	var v struct {
		D string `json:"d"`
	}
	if json.Unmarshal(wire, &v) != nil {
		return nil, "", fmt.Errorf("invalid JSON envelope")
	}
	raw, err := base64.StdEncoding.DecodeString(v.D)
	if err != nil {
		return nil, "", err
	}
	modes := []string{e.Mode}
	if e.Mode == "" || e.Mode == "auto" {
		modes = []string{"gcm", "cbc-hmac"}
	}
	for _, mode := range modes {
		switch mode {
		case "gcm":
			b, _ := aes.NewCipher(e.Key)
			g, _ := cipher.NewGCM(b)
			if len(raw) >= g.NonceSize()+g.Overhead() {
				p, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], nil)
				if err == nil {
					return p, mode, nil
				}
			}
		case "cbc-hmac":
			if len(raw) < 64 || (len(raw)-48)%16 != 0 {
				continue
			}
			payload, mac := raw[:len(raw)-32], raw[len(raw)-32:]
			h := hmac.New(sha256.New, derive(e.Key, "mac"))
			h.Write(payload)
			if !hmac.Equal(mac, h.Sum(nil)) {
				continue
			}
			b, _ := aes.NewCipher(derive(e.Key, "enc"))
			p := make([]byte, len(payload)-16)
			cipher.NewCBCDecrypter(b, payload[:16]).CryptBlocks(p, payload[16:])
			n := int(p[len(p)-1])
			if n < 1 || n > 16 || n > len(p) {
				continue
			}
			good := true
			for _, x := range p[len(p)-n:] {
				good = good && int(x) == n
			}
			if good {
				return p[:len(p)-n], mode, nil
			}
		}
	}
	return nil, "", errors.New("envelope authentication failed")
}
func ReadFrame(r io.Reader) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || n > MaxFrame {
		return nil, fmt.Errorf("invalid frame length")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}
func WriteFrame(w io.Writer, b []byte) error {
	if len(b) == 0 || len(b) > MaxFrame {
		return fmt.Errorf("invalid frame size")
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	header := h[:]
	for len(header) > 0 {
		n, err := w.Write(header)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		header = header[n:]
	}
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
