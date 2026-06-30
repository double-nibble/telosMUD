package web

// templates.go — the broker's pages (Phase 15). Minimal, dependency-free HTML: a landing page, the post-OAuth
// "return to your terminal" success page, and a generic notice (expired/cancelled link). html/template
// auto-escapes all interpolated values.

const pageTemplates = `
{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>TelosMUD</title>
<style>
 body{font:16px/1.6 system-ui,sans-serif;max-width:34rem;margin:4rem auto;padding:0 1rem;color:#1a1a1a;text-align:center}
 .logo{display:block;height:3.5rem;margin:0 auto 2rem}
 .card{border:1px solid #e2e2e2;border-radius:8px;padding:1.5rem;margin:1rem 0}
 code{background:#f2f2f2;padding:.15rem .4rem;border-radius:4px}
 h1{font-size:1.5rem}
 .ok{color:#1a7f37;font-size:1.3rem}
 .muted{color:#666}
</style></head><body><img class="logo" src="{{logoURL}}" alt="TelosMUD">{{end}}
{{define "foot"}}</body></html>{{end}}

{{define "home"}}{{template "head"}}
<h1>TelosMUD</h1>
<p class="muted">A horizontally-scalable text MUD.</p>
{{if .Configured}}<p>Connect with a MUD client and follow the sign-in link it shows you.</p>
{{else}}<p class="muted">Sign-in is not configured on this server.</p>{{end}}
{{template "foot"}}{{end}}

{{define "success"}}{{template "head"}}
<div class="card">
 <p class="ok">&check; Signed in{{if .Login}} as {{.Login}}{{end}}.</p>
 <p>Return to your terminal &mdash; you're being logged in now.</p>
</div>
<p class="muted">You can close this tab.</p>
{{template "foot"}}{{end}}

{{define "notice"}}{{template "head"}}
<div class="card"><p>{{.Message}}</p></div>
{{template "foot"}}{{end}}
`
