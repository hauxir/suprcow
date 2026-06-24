package server

import (
	"html/template"
	"net/http"
)

var waitingTmpl = template.Must(template.New("waiting").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  {{if .Refresh}}<meta http-equiv="refresh" content="2">{{end}}
  <title>suprcow — PR #{{.PR}}</title>
  <style>
    body { font-family: ui-sans-serif, system-ui, sans-serif; background:#0c0d10; color:#e7e9ee;
           display:flex; min-height:100vh; margin:0; align-items:center; justify-content:center; }
    .card { text-align:center; max-width:32rem; padding:2rem; }
    .cow { font-size:3rem; }
    h1 { font-size:1.25rem; font-weight:600; margin:1rem 0 .25rem; }
    p { color:#9aa0ad; margin:.25rem 0; }
    .pr { color:#7c93ff; }
    .spinner { margin:1.5rem auto 0; width:2rem; height:2rem; border:3px solid #2a2d36;
               border-top-color:#7c93ff; border-radius:50%; animation:spin 1s linear infinite; }
    @keyframes spin { to { transform:rotate(360deg); } }
  </style>
</head>
<body>
  <div class="card">
    <div class="cow">🐮</div>
    <h1>suprcow</h1>
    <p>Preview for <span class="pr">PR #{{.PR}}</span></p>
    <p>{{.Message}}</p>
    {{if .Refresh}}<div class="spinner"></div>{{end}}
  </div>
</body>
</html>
`))

// renderWaiting writes the branded holding page. It auto-refreshes while the
// environment is still coming up (any 5xx status), and stays put otherwise.
func (s *Server) renderWaiting(w http.ResponseWriter, pr int, message string, status int) {
	refresh := status >= 500
	if refresh {
		w.Header().Set("Retry-After", "2")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = waitingTmpl.Execute(w, struct {
		PR      int
		Message string
		Refresh bool
	}{PR: pr, Message: message, Refresh: refresh})
}
