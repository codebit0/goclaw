package http

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// checkSameOrigin validates the Origin header matches the request Host.
// Blocks cross-site WebSocket hijacking from arbitrary websites while allowing:
// - Same-origin requests (production)
// - localhost cross-port requests (Vite dev proxy: e.g. localhost:5173 → localhost:8080)
func checkSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // No Origin header = non-browser client, allow
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	originHost := u.Hostname()
	requestHost := r.Host
	// Strip port from request Host for comparison
	if h, _, err := net.SplitHostPort(requestHost); err == nil {
		requestHost = h
	}
	// Same host (ignoring port) — covers both production and localhost dev proxy
	if strings.EqualFold(originHost, requestHost) {
		return true
	}
	// Allow localhost variants talking to each other (127.0.0.1 ↔ localhost)
	if isLocalhost(originHost) && isLocalhost(requestHost) {
		return true
	}
	return false
}

func isLocalhost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// keyToVKCode maps KeyboardEvent.key names to Windows virtual key codes.
// CDP requires WindowsVirtualKeyCode for special keys to work correctly.
func keyToVKCode(key string) int {
	switch key {
	case "Backspace":
		return 8
	case "Tab":
		return 9
	case "Enter":
		return 13
	case "Shift":
		return 16
	case "Control":
		return 17
	case "Alt":
		return 18
	case "Pause":
		return 19
	case "CapsLock":
		return 20
	case "Escape":
		return 27
	case " ":
		return 32
	case "PageUp":
		return 33
	case "PageDown":
		return 34
	case "End":
		return 35
	case "Home":
		return 36
	case "ArrowLeft":
		return 37
	case "ArrowUp":
		return 38
	case "ArrowRight":
		return 39
	case "ArrowDown":
		return 40
	case "Insert":
		return 45
	case "Delete":
		return 46
	case "Meta":
		return 91
	case "ContextMenu":
		return 93
	case "F1":
		return 112
	case "F2":
		return 113
	case "F3":
		return 114
	case "F4":
		return 115
	case "F5":
		return 116
	case "F6":
		return 117
	case "F7":
		return 118
	case "F8":
		return 119
	case "F9":
		return 120
	case "F10":
		return 121
	case "F11":
		return 122
	case "F12":
		return 123
	case "NumLock":
		return 144
	case "ScrollLock":
		return 145
	default:
		// Single printable character: derive from ASCII/Unicode
		if len(key) == 1 {
			r := rune(key[0])
			// a-z → VK 0x41-0x5A (uppercase)
			if r >= 'a' && r <= 'z' {
				return int(r - 'a' + 'A')
			}
			// A-Z
			if r >= 'A' && r <= 'Z' {
				return int(r)
			}
			// 0-9
			if r >= '0' && r <= '9' {
				return int(r)
			}
		}
		return 0
	}
}

// liveViewHTML is the self-contained HTML/JS viewer for browser live view.
// %s placeholders: (1) token, (2) mode
const liveViewHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Browser Live View</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { background: #1a1a2e; display: flex; justify-content: center; align-items: center; height: 100vh; }
  #canvas { max-width: 100vw; max-height: 100vh; cursor: crosshair; }
  #status { position: fixed; top: 10px; right: 10px; color: #fff; font: 12px monospace;
            background: rgba(0,0,0,0.7); padding: 4px 8px; border-radius: 4px; }
</style>
</head>
<body>
<canvas id="canvas" width="1280" height="720"></canvas>
<div id="status">Connecting...</div>
<script>
const token = %q;
const mode = %q;
const canvas = document.getElementById('canvas');
const ctx = canvas.getContext('2d');
const status = document.getElementById('status');

const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
const ws = new WebSocket(proto + '//' + location.host + '/browser/live/' + token + '/ws');
ws.binaryType = 'arraybuffer';

ws.onopen = () => { status.textContent = 'Connected (' + mode + ')'; };
ws.onclose = () => { status.textContent = 'Disconnected'; };

ws.onmessage = (e) => {
  if (e.data instanceof ArrayBuffer) {
    const blob = new Blob([e.data], {type: 'image/jpeg'});
    const img = new Image();
    img.onload = () => {
      canvas.width = img.width;
      canvas.height = img.height;
      ctx.drawImage(img, 0, 0);
      URL.revokeObjectURL(img.src);
    };
    img.src = URL.createObjectURL(blob);
  }
};

if (mode === 'takeover') {
  canvas.addEventListener('mousedown', (e) => {
    const r = canvas.getBoundingClientRect();
    ws.send(JSON.stringify({type:'mousePressed', x:e.clientX-r.left, y:e.clientY-r.top, button:'left', clickCount:1}));
  });
  canvas.addEventListener('mouseup', (e) => {
    const r = canvas.getBoundingClientRect();
    ws.send(JSON.stringify({type:'mouseReleased', x:e.clientX-r.left, y:e.clientY-r.top, button:'left', clickCount:1}));
  });
  canvas.addEventListener('mousemove', (e) => {
    if (e.buttons === 0) return;
    const r = canvas.getBoundingClientRect();
    ws.send(JSON.stringify({type:'mouseMoved', x:e.clientX-r.left, y:e.clientY-r.top}));
  });
  document.addEventListener('keydown', (e) => {
    ws.send(JSON.stringify({type:'keyDown', key:e.key, code:e.code, text:e.key.length===1?e.key:'', modifiers:0}));
  });
  document.addEventListener('keyup', (e) => {
    ws.send(JSON.stringify({type:'keyUp', key:e.key, code:e.code}));
  });
}
</script>
</body>
</html>`
