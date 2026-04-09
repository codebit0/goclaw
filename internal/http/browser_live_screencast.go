package http

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nextlevelbuilder/goclaw/pkg/browser"
)

// inputEvent holds a decoded browser input event for dispatch to CDP.
type inputEvent struct {
	typ                     string
	cx, cy                  float64
	btn                     string
	clickCount, mod         int
	deltaX, deltaY          float64
	key, code, text         string
	vkCode                  int
}

// runScreencastLoop is the shared screencast + input relay loop used by both
// authenticated (chat panel) and token-based (share) WS handlers.
func (h *BrowserLiveHandler) runScreencastLoop(conn *websocket.Conn, page browser.Page, mode string, logID string) {
	frameCh := make(chan browser.ScreencastFrame, 20) // 2 seconds buffer at 10fps

	// Activate (bring to front) so CDP screencast captures this page.
	// Background tabs don't generate screencast frames.
	_ = page.Activate()

	vpW, vpH := h.getManager().ViewportSize()
	const screencastFPS = 10
	const screencastQuality = 60
	if err := page.StartScreencast(screencastFPS, screencastQuality, vpW, vpH, frameCh); err != nil {
		conn.WriteMessage(websocket.TextMessage, fmt.Appendf(nil, `{"error":"screencast start failed: %v"}`, err))
		return
	}

	// Coordinate mapping state
	var (
		viewportW float64 = float64(vpW)
		viewportH float64 = float64(vpH)
		dimsMu    sync.Mutex
	)

	// Image dimension tracking for coordinate mapping (shared with frame sender goroutine).
	var imageW, imageH float64
	var imgDimsMu sync.Mutex

	// Frame sender goroutine
	done := make(chan struct{})
	stopFrames := make(chan struct{})
	go func() {
		defer close(done)
		viewportSent := false
		for {
			var frame browser.ScreencastFrame
			var ok bool
			select {
			case frame, ok = <-frameCh:
				if !ok {
					return
				}
			case <-stopFrames:
				return
			}
			if frame.Metadata.DeviceWidth > 0 {
				dimsMu.Lock()
				viewportW = frame.Metadata.DeviceWidth
				viewportH = frame.Metadata.DeviceHeight
				dimsMu.Unlock()
				if !viewportSent {
					msg, _ := json.Marshal(map[string]any{
						"viewport": map[string]float64{
							"w": frame.Metadata.DeviceWidth,
							"h": frame.Metadata.DeviceHeight,
						},
					})
					conn.WriteMessage(websocket.TextMessage, msg)
					viewportSent = true
				}
			}
			// Track image dimensions from JPEG for coordinate mapping.
			// Screencast JPEG size = viewport scaled to fit maxW x maxH.
			if frame.Metadata.DeviceWidth > 0 {
				imgDimsMu.Lock()
				// Screencast output is scaled to fit within maxW x maxH while preserving aspect ratio.
				scale := min(float64(vpW)/frame.Metadata.DeviceWidth, float64(vpH)/frame.Metadata.DeviceHeight)
				imageW = frame.Metadata.DeviceWidth * scale
				imageH = frame.Metadata.DeviceHeight * scale
				imgDimsMu.Unlock()
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, frame.Data); err != nil {
				h.logger.Warn("screencast frame write failed", "id", logID, "error", err)
				return
			}
		}
	}()

	// mapCoords maps image pixel coordinates to CSS viewport coordinates.
	// Frontend sends coords in native image pixel space (canvas.width x canvas.height).
	// Screencast JPEG dimensions may differ from the CSS viewport due to device pixel
	// ratio and fit-to-maxW/maxH scaling. We use the ratio from frame metadata.
	mapCoords := func(imgX, imgY float64) (float64, float64) {
		dimsMu.Lock()
		vw, vh := viewportW, viewportH
		dimsMu.Unlock()
		imgDimsMu.Lock()
		iw, ih := imageW, imageH
		imgDimsMu.Unlock()
		if iw > 0 && ih > 0 {
			return imgX * vw / iw, imgY * vh / ih
		}
		// Fallback: assume image matches viewport
		return imgX, imgY
	}

	cdpButton := func(btn int) string {
		switch btn {
		case 0:
			return "left"
		case 1:
			return "middle"
		case 2:
			return "right"
		default:
			return "none"
		}
	}

	// Input dispatch via single goroutine + channel.
	// Buffered channel absorbs bursts; mousemove dropped on backpressure, clicks never dropped.
	inputCh := make(chan inputEvent, 32)
	inputDone := make(chan struct{})

	go func() {
		defer close(inputDone)
		for ev := range inputCh {
			dispatchInputEvent(page, ev)
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}

		if mode != "takeover" {
			continue
		}

		ev, ok := parseInputMessage(msg, mapCoords, cdpButton, page)
		if !ok {
			continue
		}
		if ev.typ == "" {
			// navigation was handled inline
			continue
		}

		// Mousemove: drop immediately on backpressure (high-frequency, stale events are useless).
		// All other events (clicks, keys, scroll): use a short timeout rather than blocking
		// indefinitely — a 500ms stale click is better dropped than blocking the WS read loop
		// which would also stall screencast frame delivery.
		droppable := ev.typ == "mousemove" || ev.typ == "mouseMoved"
		if droppable {
			select {
			case inputCh <- ev:
			default: // drop stale mousemove
			}
		} else {
			select {
			case inputCh <- ev:
			case <-time.After(500 * time.Millisecond):
				// drop stale click/key rather than blocking the WS read loop
			}
		}
	}

	close(inputCh)
	<-inputDone

	// Unsubscribe this viewer's channel. CDP screencast stops only when last viewer leaves.
	page.StopScreencastCh(frameCh)
	close(stopFrames)
	<-done
}

// parseInputMessage decodes a raw WS message into an inputEvent.
// Returns (event, false) on JSON error, (zero-event with typ=="", true) for nav commands handled inline.
func parseInputMessage(msg []byte, mapCoords func(float64, float64) (float64, float64), cdpButton func(int) string, page browser.Page) (inputEvent, bool) {
	var raw struct {
		Type       string  `json:"type"`
		X          float64 `json:"x"`
		Y          float64 `json:"y"`
		Button     int     `json:"button"`
		ButtonName string  `json:"buttonName"`
		ClickCount int     `json:"clickCount"`
		DeltaX     float64 `json:"deltaX"`
		DeltaY     float64 `json:"deltaY"`
		Key        string  `json:"key"`
		Code       string  `json:"code"`
		Text       string  `json:"text"`
		Modifiers  int     `json:"modifiers"`
		VKCode     int     `json:"vkCode"`
		Shift      bool    `json:"shift"`
		Ctrl       bool    `json:"ctrl"`
		Alt        bool    `json:"alt"`
		Meta       bool    `json:"meta"`
	}
	if err := json.Unmarshal(msg, &raw); err != nil {
		return inputEvent{}, false
	}

	// Navigation commands — handled inline, return sentinel.
	if raw.Type == "nav" {
		switch raw.Key {
		case "back":
			page.Eval(`() => history.back()`)
		case "forward":
			page.Eval(`() => history.forward()`)
		case "reload":
			page.Eval(`() => location.reload()`)
		}
		return inputEvent{}, true
	}

	cssX, cssY := mapCoords(raw.X, raw.Y)
	btn := cdpButton(raw.Button)
	if raw.ButtonName != "" {
		btn = raw.ButtonName
	}

	mod := raw.Modifiers
	if mod == 0 {
		if raw.Alt {
			mod |= 1
		}
		if raw.Ctrl {
			mod |= 2
		}
		if raw.Meta {
			mod |= 4
		}
		if raw.Shift {
			mod |= 8
		}
	}

	vkCode := raw.VKCode
	if vkCode == 0 {
		vkCode = keyToVKCode(raw.Key)
	}

	text := raw.Text
	if text == "" && (raw.Type == "keydown" || raw.Type == "keyDown") && len(raw.Key) == 1 {
		text = raw.Key
	}

	return inputEvent{
		typ: raw.Type, cx: cssX, cy: cssY, btn: btn,
		clickCount: raw.ClickCount, mod: mod,
		deltaX: raw.DeltaX, deltaY: raw.DeltaY,
		key: raw.Key, code: raw.Code, text: text,
		vkCode: vkCode,
	}, true
}

// dispatchInputEvent sends a single input event to the CDP page.
func dispatchInputEvent(page browser.Page, ev inputEvent) {
	switch ev.typ {
	case "mousedown":
		page.DispatchMouseEvent("mousePressed", ev.cx, ev.cy, ev.btn, 1)
	case "mouseup":
		page.DispatchMouseEvent("mouseReleased", ev.cx, ev.cy, ev.btn, 1)
	case "click":
		page.DispatchMouseEvent("mousePressed", ev.cx, ev.cy, ev.btn, 1)
		time.Sleep(30 * time.Millisecond)
		page.DispatchMouseEvent("mouseReleased", ev.cx, ev.cy, ev.btn, 1)
	case "dblclick":
		page.DispatchMouseEvent("mousePressed", ev.cx, ev.cy, ev.btn, 1)
		page.DispatchMouseEvent("mouseReleased", ev.cx, ev.cy, ev.btn, 1)
		time.Sleep(10 * time.Millisecond)
		page.DispatchMouseEvent("mousePressed", ev.cx, ev.cy, ev.btn, 2)
		page.DispatchMouseEvent("mouseReleased", ev.cx, ev.cy, ev.btn, 2)
	case "mousemove":
		page.DispatchMouseEvent("mouseMoved", ev.cx, ev.cy, "none", 0)
	case "scroll", "mouseWheel":
		page.DispatchScrollEvent(ev.cx, ev.cy, ev.deltaX, ev.deltaY)
	case "mousePressed", "mouseReleased", "mouseMoved":
		page.DispatchMouseEvent(ev.typ, ev.cx, ev.cy, ev.btn, ev.clickCount)
	case "keydown", "keyDown":
		page.DispatchKeyEvent("keyDown", ev.key, ev.code, "", ev.mod, ev.vkCode)
		if ev.text != "" {
			page.DispatchKeyEvent("char", ev.key, ev.code, ev.text, ev.mod, ev.vkCode)
		}
	case "keyup", "keyUp":
		page.DispatchKeyEvent("keyUp", ev.key, ev.code, "", ev.mod, ev.vkCode)
	}
}
