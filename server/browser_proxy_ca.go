package server

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"time"
)

func browserProxyCAPin(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("recording proxy CA is not a PEM certificate")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	now := time.Now()
	if !ca.IsCA || now.Before(ca.NotBefore) || now.After(ca.NotAfter) {
		return "", errors.New("recording proxy CA is invalid or expired")
	}
	pin := sha256.Sum256(ca.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(pin[:]), nil
}
