// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 yi-protect contributors

// webui.go: the firmware settings page (plain HTTP, default :80), via mediad's socket.
// Saving pins a key (persisted in mediad.conf, immune to Protect's re-asserts);
// "Let Protect control" unpins it.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// webuiControl describes one switch on the page. Keys are mediad control keys.
type webuiControl struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Help  string `json:"help"`
	Kind  string `json:"kind"` // "toggle", "level" (0..Max) or "range"
	Max   int    `json:"max"`
}

var webuiDenoise = []webuiControl{
	{"nr2d", "Spatial denoise", "Smooths grain within each frame (ISP 2D noise reduction). Keeps motion sharp.", "toggle", 1},
	{"cnr", "Chroma denoise", "Removes colour speckle (ISP chroma noise reduction).", "toggle", 1},
	{"tdf", "Temporal denoise", "Averages across frames (ISP 3DNR). Strongest in low light, but can leave a ghost behind moving subjects.", "toggle", 1},
	{"venc3d", "Encoder 3D filter", "The video encoder's own temporal filter strength, 0 (off) to 511. The stock firmware's levels 1-3 equal about 1-6, which is barely visible; higher values smooth more and smear motion more.", "range", 511},
	{"denoise", "Denoise strength", "Threshold ramp for spatial and temporal denoise. 0 uses the camera tuning's own strength.", "range", 100},
}

type webuiState struct {
	webuiControl
	Value  int  `json:"value"`
	Pinned bool `json:"pinned"`
	OK     bool `json:"ok"`
}

// mediadInt sends `<verb> <key>` and parses the "ok <key> <n>" reply.
func mediadInt(verb, key string) (int, error) {
	reply, err := mediadCommand(verb + " " + key)
	if err != nil {
		return 0, err
	}
	f := strings.Fields(reply)
	if len(f) < 3 || f[0] != "ok" {
		return 0, fmt.Errorf("mediad: %s", reply)
	}
	return strconv.Atoi(f[2])
}

func webuiFind(key string) (webuiControl, bool) {
	for _, c := range webuiDenoise {
		if c.Key == key {
			return c, true
		}
	}
	return webuiControl{}, false
}

func webuiReadAll() []webuiState {
	out := make([]webuiState, 0, len(webuiDenoise))
	for _, c := range webuiDenoise {
		s := webuiState{webuiControl: c}
		v, err := mediadInt("get", c.Key)
		if err == nil {
			p, perr := mediadInt("pinned", c.Key)
			s.Value, s.Pinned, s.OK = v, perr == nil && p == 1, true
		}
		out = append(out, s)
	}
	return out
}

func webuiJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func handleWebuiDenoise(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		webuiJSON(w, http.StatusOK, webuiReadAll())
	case http.MethodPost:
		var req struct {
			Key   string `json:"key"`
			Value int    `json:"value"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil {
			webuiJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		c, ok := webuiFind(req.Key)
		if !ok || req.Value < 0 || req.Value > c.Max {
			webuiJSON(w, http.StatusBadRequest, map[string]string{"error": "bad key or value"})
			return
		}
		reply, err := mediadCommand(fmt.Sprintf("pin %s %d", req.Key, req.Value))
		if err != nil || !strings.HasPrefix(reply, "ok ") {
			log.Printf("webui: pin %s %d: %q %v", req.Key, req.Value, reply, err)
			webuiJSON(w, http.StatusBadGateway, map[string]string{"error": "camera did not accept the setting"})
			return
		}
		log.Printf("webui: %s = %d (pinned)", req.Key, req.Value)
		webuiJSON(w, http.StatusOK, webuiReadAll())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleWebuiUnpin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil {
		webuiJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if _, ok := webuiFind(req.Key); !ok {
		webuiJSON(w, http.StatusBadRequest, map[string]string{"error": "bad key"})
		return
	}
	reply, err := mediadCommand("unpin " + req.Key)
	if err != nil || !strings.HasPrefix(reply, "ok ") {
		log.Printf("webui: unpin %s: %q %v", req.Key, reply, err)
		webuiJSON(w, http.StatusBadGateway, map[string]string{"error": "camera did not accept the change"})
		return
	}
	log.Printf("webui: %s handed back to Protect", req.Key)
	webuiJSON(w, http.StatusOK, webuiReadAll())
}

func webuiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/denoise", handleWebuiDenoise)
	mux.HandleFunc("/api/denoise/unpin", handleWebuiUnpin)
	// Any other GET path serves the page too (a bookmarked /index.html etc.
	// should never land on "404 page not found").
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, webuiPage)
	})
	return mux
}

// runWebUI serves the settings page forever; errors are logged, not fatal.
func runWebUI(addr string) {
	log.Printf("webui: listening on %s", addr)
	if err := http.ListenAndServe(addr, webuiHandler()); err != nil {
		log.Printf("webui: listener stopped: %v", err)
	}
}

const webuiPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Camera settings</title>
<style>
:root{--bg:#f6f7f9;--card:#fff;--text:#1b1f24;--muted:#5d6670;--line:#e3e6ea;--accent:#2563eb;--on:#16a34a;--warn:#b45309}
@media (prefers-color-scheme:dark){:root{--bg:#111418;--card:#1a1f25;--text:#e8ebef;--muted:#9aa3ad;--line:#2a3139;--accent:#60a5fa;--on:#4ade80;--warn:#fbbf24}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:15px/1.45 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
main{max-width:640px;margin:0 auto;padding:24px 16px 48px}
h1{font-size:22px;margin:0 0 4px}p.sub{margin:0 0 20px;color:var(--muted)}
section{background:var(--card);border:1px solid var(--line);border-radius:12px;overflow:hidden}
section h2{font-size:13px;letter-spacing:.06em;text-transform:uppercase;color:var(--muted);margin:0;padding:14px 16px;border-bottom:1px solid var(--line)}
.row{padding:14px 16px;border-bottom:1px solid var(--line);display:grid;grid-template-columns:1fr auto;gap:6px 16px;align-items:center}
.row:last-child{border-bottom:0}
.name{font-weight:600}.help{grid-column:1/-1;color:var(--muted);font-size:13px}
.meta{grid-column:1/-1;display:flex;gap:10px;align-items:center;font-size:12px;color:var(--muted)}
.badge{padding:1px 8px;border-radius:99px;border:1px solid var(--line)}
.badge.pin{color:var(--accent);border-color:var(--accent)}
button.link{background:none;border:0;color:var(--accent);cursor:pointer;padding:0;font:inherit;font-size:12px}
.sw{position:relative;width:46px;height:26px}.sw input{opacity:0;width:0;height:0}
.sw span{position:absolute;inset:0;background:var(--line);border-radius:99px;transition:.15s;cursor:pointer}
.sw span:before{content:"";position:absolute;width:20px;height:20px;left:3px;top:3px;background:#fff;border-radius:50%;transition:.15s}
.sw input:checked+span{background:var(--on)}.sw input:checked+span:before{transform:translateX(20px)}
.sw input:focus-visible+span{outline:2px solid var(--accent);outline-offset:2px}
select,input[type=range]{font:inherit;color:var(--text);background:var(--card);border:1px solid var(--line);border-radius:8px;padding:4px 8px}
input[type=range]{padding:0;width:140px;accent-color:var(--accent)}
.val{min-width:2.5em;text-align:right;font-variant-numeric:tabular-nums}
.ctl{display:flex;align-items:center;gap:8px}
#msg{min-height:1.5em;margin:12px 2px 0;font-size:13px;color:var(--muted)}#msg.err{color:var(--warn)}
</style></head><body><main>
<h1>Camera settings</h1>
<p class="sub">Changes apply live and are saved on the camera. A saved value overrides what Protect sends.</p>
<section><h2>Noise reduction</h2><div id="rows">Loading&hellip;</div></section>
<div id="msg" role="status"></div>
</main><script>
const rows=document.getElementById('rows'),msg=document.getElementById('msg');
function say(t,err){msg.textContent=t;msg.className=err?'err':''}
async function call(url,body){
  const r=await fetch(url,body?{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}:{});
  const j=await r.json().catch(()=>({error:'bad reply'}));
  if(!r.ok)throw new Error(j.error||r.statusText);return j}
function control(c){
  if(c.kind==='toggle')return '<label class="sw"><input type="checkbox" data-k="'+c.key+'"'+(c.value?' checked':'')+' aria-label="'+c.label+'"><span></span></label>';
  if(c.kind==='level'){let o='';for(let i=0;i<=c.max;i++)o+='<option value="'+i+'"'+(i===c.value?' selected':'')+'>'+(i?'Level '+i:'Off')+'</option>';
    return '<select data-k="'+c.key+'" aria-label="'+c.label+'">'+o+'</select>'}
  return '<div class="ctl"><input type="range" min="0" max="'+c.max+'" value="'+c.value+'" data-k="'+c.key+'" aria-label="'+c.label+'"><span class="val">'+c.value+'</span></div>'}
function render(list){
  rows.innerHTML=list.map(c=>'<div class="row"><div class="name">'+c.label+'</div>'+(c.ok?control(c):'<span class="help">unavailable</span>')+
    '<div class="help">'+c.help+'</div><div class="meta">'+(c.pinned?'<span class="badge pin">Saved on this page</span><button class="link" data-unpin="'+c.key+'">Let Protect control</button>':'<span class="badge">Protect controls</span>')+'</div></div>').join('');
  rows.querySelectorAll('[data-k]').forEach(el=>{
    const ev=el.type==='range'?'change':'change';
    if(el.type==='range')el.addEventListener('input',()=>el.nextElementSibling.textContent=el.value);
    el.addEventListener(ev,()=>save(el.dataset.k,el.type==='checkbox'?(el.checked?1:0):+el.value))});
  rows.querySelectorAll('[data-unpin]').forEach(b=>b.addEventListener('click',()=>unpin(b.dataset.unpin)))}
async function load(){try{render(await call('/api/denoise'));say('')}catch(e){rows.textContent='Could not reach the camera ('+e.message+').'}}
async function save(k,v){say('Saving…');try{render(await call('/api/denoise',{key:k,value:v}));say('Saved.')}catch(e){say('Not saved: '+e.message,1);load()}}
async function unpin(k){try{render(await call('/api/denoise/unpin',{key:k}));say('Protect now controls this setting.')}catch(e){say(e.message,1)}}
load();
</script></body></html>
`
