package web

// templates.go — the website's server-rendered pages (Phase 14.7). Minimal, dependency-free HTML; the
// dynamic chargen form (14.8) extends the dashboard. html/template auto-escapes all interpolated values.

const pageTemplates = `
{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>TelosMUD</title>
<style>
 body{font:16px/1.5 system-ui,sans-serif;max-width:42rem;margin:3rem auto;padding:0 1rem;color:#1a1a1a}
 a.btn,button{display:inline-block;background:#2d6cdf;color:#fff;border:0;border-radius:6px;padding:.6rem 1rem;font:inherit;text-decoration:none;cursor:pointer}
 a.muted{color:#666}
 code{background:#f2f2f2;padding:.15rem .4rem;border-radius:4px;font-size:1.1em}
 .card{border:1px solid #e2e2e2;border-radius:8px;padding:1rem 1.25rem;margin:1rem 0}
 h1{font-size:1.6rem}
 .logo{display:block;height:3.5rem;margin:0 0 1.5rem}
</style></head><body><a href="/"><img class="logo" src="{{logoURL}}" alt="TelosMUD"></a>{{end}}
{{define "foot"}}</body></html>{{end}}

{{define "home"}}{{template "head"}}
<h1>TelosMUD</h1>
<p>A horizontally-scalable text MUD.</p>
{{if .Configured}}<p><a class="btn" href="/login">Sign in with GitHub</a></p>
{{else}}<p class="muted">Sign-in is not configured on this server.</p>{{end}}
{{template "foot"}}{{end}}

{{define "dashboard"}}{{template "head"}}
<h1>Welcome{{if .Name}}, {{.Name}}{{end}}</h1>
<div class="card">
 <h2 style="font-size:1.2rem;margin-top:0">Your characters</h2>
 {{if .Characters}}<ul>{{range .Characters}}<li>{{.Name}}{{if .ZoneRef}} <span class="muted">({{.ZoneRef}})</span>{{end}}</li>{{end}}</ul>
 {{else}}<p class="muted">No characters yet.</p>{{end}}
</div>
<form method="post" action="/play"><button type="submit">Play &rarr; get a link code</button></form>
<form method="post" action="/logout" style="margin-top:2rem"><button class="muted" type="submit" style="background:none;color:#666;padding:0;border:0;text-decoration:underline">Sign out</button></form>
{{template "foot"}}{{end}}

{{define "play"}}{{template "head"}}
<h1>Ready to play</h1>
<div class="card">
 <p>Connect{{if .GateHint}} to <code>{{.GateHint}}</code>{{end}} and enter:</p>
 <p><code>connect {{.Code}}</code></p>
 <p class="muted">This code is single-use and expires in {{.TTLMin}} minutes.</p>
</div>
<p><a class="muted" href="/dashboard">&larr; Back to dashboard</a></p>
{{template "foot"}}{{end}}
`
