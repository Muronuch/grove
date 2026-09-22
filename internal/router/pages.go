package router

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/Muronuch/grove/internal/meta"
)

type page struct {
	Title          string
	Body           string
	Hints          []string
	Links          map[string]string
	Refresh        bool
	RefreshSeconds int
}

const pageCSS = `
:root{color-scheme:light dark}
*{box-sizing:border-box}
body{font:15px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;
     margin:0;padding:3rem 1.25rem;display:flex;justify-content:center;
     background:#fbfbfc;color:#16181d}
main{width:100%;max-width:44rem}
h1{font-size:1.15rem;margin:0 0 .85rem;font-weight:650;letter-spacing:-.01em}
p{margin:.55rem 0}
code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.86em;
     background:#eceef2;border-radius:4px;padding:.12em .38em}
pre{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.8rem;
    background:#eceef2;border-radius:8px;padding:.7rem .85rem;overflow:auto;margin:.7rem 0}
pre.err,p.err{color:#8b1a1a;background:#fdf0f0}
ul{margin:.5rem 0;padding-left:1.15rem}
li{margin:.25rem 0}
a{color:#1a56d6}
.hints{margin-top:1.4rem;padding-top:1rem;border-top:1px solid #e3e5ea;color:#585d68;font-size:.88rem}
.badge{display:inline-block;font-size:.72rem;text-transform:uppercase;letter-spacing:.04em;
       border-radius:999px;padding:.12em .6em;background:#e3e5ea;color:#3a3f4a;vertical-align:.1em}
.foot{margin-top:2rem;font-size:.78rem;color:#8a8f99}
table{border-collapse:collapse;width:100%;margin:.6rem 0;font-size:.88rem}
th,td{text-align:left;padding:.4rem .6rem;border-bottom:1px solid #e3e5ea}
th{font-weight:600;color:#585d68;font-size:.78rem;text-transform:uppercase;letter-spacing:.03em}
@media (prefers-color-scheme:dark){
  body{background:#101216;color:#e6e8ec}
  code,pre{background:#1c1f26}
  pre.err,p.err{color:#ff9d9d;background:#2a1717}
  .hints{border-color:#262a33;color:#989ea9}
  .badge{background:#262a33;color:#b9bfc9}
  th,td{border-color:#262a33}
  th{color:#989ea9}
  a{color:#7aa7ff}
  .foot{color:#6c727c}
}
`

func writePage(w io.Writer, p page) {
	refresh := ""
	if p.Refresh {
		n := p.RefreshSeconds
		if n == 0 {
			n = 1
		}
		refresh = fmt.Sprintf(`<meta http-equiv="refresh" content="%d">`, n)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">%s
<title>%s · %s</title><style>%s</style></head><body><main>`,
		refresh, htmlEscape(p.Title), meta.Name, pageCSS)
	fmt.Fprintf(&b, "<h1>%s</h1>%s", htmlEscape(p.Title), p.Body)
	if len(p.Links) > 0 {
		b.WriteString("<ul>")
		keys := make([]string, 0, len(p.Links))
		for k := range p.Links {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, `<li><a href="%s">%s</a> — %s</li>`,
				htmlEscape(p.Links[k]), htmlEscape(p.Links[k]), htmlEscape(k))
		}
		b.WriteString("</ul>")
	}
	if len(p.Hints) > 0 {
		b.WriteString(`<div class="hints">`)
		for _, h := range p.Hints {
			fmt.Fprintf(&b, "<p>%s</p>", h)
		}
		b.WriteString("</div>")
	}
	fmt.Fprintf(&b, `<p class="foot">%s router %s</p></main></body></html>`, meta.Name, meta.VersionString())
	io.WriteString(w, b.String())
}

func (p *Proxy) unknownHost(w http.ResponseWriter, r *http.Request, host string) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set(ErrorHeader, "router")
	w.WriteHeader(http.StatusNotFound)

	project := guessProject(host)
	envs := p.store.ProjectEnvs(project)
	body := fmt.Sprintf("<p>No env answers to <code>%s</code>.</p>", htmlEscape(host))

	if len(envs) > 0 {
		body += fmt.Sprintf("<p>Envs of project <b>%s</b>:</p>", htmlEscape(project))
		body += envTable(envs)
	} else {
		all := p.store.AllEnvs()
		if len(all) == 0 {
			body += "<p>The router has no envs at all. Start one with <code>" + meta.Name + " new &lt;branch&gt;</code>.</p>"
		} else {
			refs := make([]*EnvRoute, 0, len(all))
			for i := range all {
				refs = append(refs, &all[i])
			}
			body += "<p>Known envs:</p>" + envTable(refs)
		}
	}
	writePage(w, page{
		Title: "404 · unknown host",
		Body:  body,
		Hints: []string{"<code>" + meta.Name + " ls</code> lists every env and its URL."},
	})
}

func envTable(envs []*EnvRoute) string {
	var b strings.Builder
	b.WriteString("<table><tr><th>env</th><th>slot</th><th>state</th><th>url</th></tr>")
	for _, e := range envs {
		url := ""
		for _, h := range e.Hosts {
			if strings.Count(h, ".") == strings.Count(shortest(e.Hosts), ".") {
				url = "http://" + h
				break
			}
		}
		fmt.Fprintf(&b, `<tr><td>%s</td><td>%d</td><td><span class="badge">%s</span></td><td><a href="%s">%s</a></td></tr>`,
			htmlEscape(e.Env), e.Slot, htmlEscape(string(e.State)), htmlEscape(url), htmlEscape(url))
	}
	b.WriteString("</table>")
	return b.String()
}

func shortest(hosts []string) string {
	best := ""
	for _, h := range hosts {
		if best == "" || strings.Count(h, ".") < strings.Count(best, ".") {
			best = h
		}
	}
	return best
}

func guessProject(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

var ErrorHeader = meta.HeaderName("Error")
