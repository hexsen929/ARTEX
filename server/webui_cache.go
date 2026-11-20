package server

import (
	"net/http"
	"strings"
)

func setWebUICacheHeaders(w http.ResponseWriter, name string) {
	if strings.HasPrefix(strings.TrimPrefix(name, "/"), "_next/static/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
}
