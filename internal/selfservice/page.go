package selfservice

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.identify(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}
