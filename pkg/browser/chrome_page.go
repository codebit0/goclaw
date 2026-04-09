package browser

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

// ---------------------------------------------------------------------------
// ChromePage wraps *rod.Page to implement the Page interface.
// ---------------------------------------------------------------------------

type ChromePage struct {
	page           *rod.Page
	errors         []*JSError
	errMu          sync.Mutex
	screencastMu   sync.Mutex
	screencastSubs map[chan<- ScreencastFrame]struct{} // fan-out: multiple WS viewers per page
	screencastOn   bool                               // true if CDP screencast + event listener are active
	screencastStop  context.CancelFunc                 // cancels the EachEvent listener goroutine
}

func newChromePage(p *rod.Page) *ChromePage {
	return &ChromePage{page: p}
}

// RodPage returns the underlying *rod.Page for Chrome-specific operations.
func (p *ChromePage) RodPage() *rod.Page { return p.page }

// --- Navigation ---

func (p *ChromePage) Navigate(url string) error {
	return p.page.Navigate(url)
}

func (p *ChromePage) WaitStable(d time.Duration) error {
	return p.page.WaitStable(d)
}

func (p *ChromePage) Info() (*PageInfo, error) {
	info, err := p.page.Info()
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil
	}
	return &PageInfo{URL: info.URL, Title: info.Title}, nil
}

func (p *ChromePage) TargetID() string {
	return string(p.page.TargetID)
}

func (p *ChromePage) Close() error {
	return p.page.Close()
}

func (p *ChromePage) Activate() error {
	_, err := p.page.Activate()
	return err
}

// --- Content ---

func (p *ChromePage) GetAXTree() ([]*proto.AccessibilityAXNode, error) {
	result, err := proto.AccessibilityGetFullAXTree{}.Call(p.page)
	if err != nil {
		return nil, err
	}
	return result.Nodes, nil
}

func (p *ChromePage) GetFrameTree() ([]FrameInfo, error) {
	result, err := proto.PageGetFrameTree{}.Call(p.page)
	if err != nil {
		return nil, err
	}
	if result.FrameTree == nil {
		return nil, nil
	}

	// Build OOPIF target map (URL → targetID) before flattening
	oopifByURL := p.discoverOOPIFsByURL()

	var frames []FrameInfo
	flattenFrameTree(result.FrameTree, "", 0, oopifByURL, &frames)
	return frames, nil
}

// discoverOOPIFsByURL finds all cross-origin iframe targets and indexes them by URL.
// Chrome creates separate targets (type "iframe") for OOPIFs. We match them to
// frames in the tree by URL since OOPIFs don't carry an OpenerID.
func (p *ChromePage) discoverOOPIFsByURL() map[string]string {
	browser := p.page.Browser()
	if browser == nil {
		return nil
	}
	targets, err := proto.TargetGetTargets{}.Call(browser)
	if err != nil {
		return nil
	}

	result := make(map[string]string)
	for _, t := range targets.TargetInfos {
		if string(t.Type) == "iframe" {
			result[t.URL] = string(t.TargetID)
		}
	}
	return result
}

// flattenFrameTree recursively converts the CDP FrameTree into a flat slice.
// oopifByURL maps iframe URLs to their OOPIF target IDs for cross-origin annotation.
func flattenFrameTree(tree *proto.PageFrameTree, parentID string, depth int, oopifByURL map[string]string, out *[]FrameInfo) {
	if tree.Frame == nil {
		return
	}
	f := tree.Frame
	fi := FrameInfo{
		FrameID:  string(f.ID),
		ParentID: parentID,
		URL:      f.URL,
		Name:     f.Name,
		Origin:   f.SecurityOrigin,
		Depth:    depth,
	}
	// Annotate cross-origin frames with their OOPIF target ID
	if tid, ok := oopifByURL[f.URL]; ok {
		fi.CrossOrigin = true
		fi.OOPIFTarget = tid
	}
	*out = append(*out, fi)
	for _, child := range tree.ChildFrames {
		flattenFrameTree(child, string(f.ID), depth+1, oopifByURL, out)
	}
}

func (p *ChromePage) GetAXTreeForFrame(frameID string) ([]*proto.AccessibilityAXNode, error) {
	// First try same-origin access via FrameID parameter
	result, err := proto.AccessibilityGetFullAXTree{
		FrameID: proto.PageFrameID(frameID),
	}.Call(p.page)
	if err == nil && len(result.Nodes) > 0 {
		return result.Nodes, nil
	}

	// Same-origin failed or returned empty — try OOPIF approach:
	// Attach to the iframe's target and get its AX tree directly.
	browser := p.page.Browser()
	if browser == nil {
		if err != nil {
			return nil, err
		}
		return result.Nodes, nil
	}

	iframePage, attachErr := browser.PageFromTarget(proto.TargetTargetID(frameID))
	if attachErr != nil {
		// frameID wasn't a target ID — return original result
		if err != nil {
			return nil, fmt.Errorf("frame %s: same-origin empty, OOPIF attach failed: %v", frameID, attachErr)
		}
		return result.Nodes, nil
	}

	// Get AX tree from the OOPIF target (no FrameID needed — the whole target IS the frame)
	oopifResult, oopifErr := proto.AccessibilityGetFullAXTree{}.Call(iframePage)
	if oopifErr != nil {
		return nil, fmt.Errorf("get OOPIF AX tree for %s: %w", frameID, oopifErr)
	}
	return oopifResult.Nodes, nil
}

func (p *ChromePage) Screenshot(fullPage bool, opts *proto.PageCaptureScreenshot) ([]byte, error) {
	return p.page.Screenshot(fullPage, opts)
}

func (p *ChromePage) Eval(js string) (*proto.RuntimeRemoteObject, error) {
	return p.page.Eval(js)
}

// --- Input ---

func (p *ChromePage) KeyboardPress(key input.Key) error {
	return p.page.Keyboard.Press(key)
}

// --- Raw CDP Input dispatch ---

func (p *ChromePage) DispatchMouseEvent(typ string, x, y float64, button string, clickCount int) error {
	return proto.InputDispatchMouseEvent{
		Type:       proto.InputDispatchMouseEventType(typ),
		X:          x,
		Y:          y,
		Button:     proto.InputMouseButton(button),
		ClickCount: clickCount,
	}.Call(p.page)
}

func (p *ChromePage) DispatchScrollEvent(x, y, deltaX, deltaY float64) error {
	return proto.InputDispatchMouseEvent{
		Type:   proto.InputDispatchMouseEventTypeMouseWheel,
		X:      x,
		Y:      y,
		DeltaX: deltaX,
		DeltaY: deltaY,
	}.Call(p.page)
}

func (p *ChromePage) DispatchKeyEvent(typ string, key, code, text string, modifiers int, vkCode int) error {
	return proto.InputDispatchKeyEvent{
		Type:                  proto.InputDispatchKeyEventType(typ),
		Key:                   key,
		Code:                  code,
		Text:                  text,
		Modifiers:             modifiers,
		WindowsVirtualKeyCode: vkCode,
	}.Call(p.page)
}

// --- DOM resolution ---

func (p *ChromePage) ResolveBackendNode(backendNodeID int) (Element, error) {
	bid := proto.DOMBackendNodeID(backendNodeID)
	resolved, err := proto.DOMResolveNode{BackendNodeID: bid}.Call(p.page)
	if err != nil {
		return nil, err
	}
	el, err := p.page.ElementFromObject(resolved.Object)
	if err != nil {
		return nil, err
	}
	return &ChromeElement{el: el}, nil
}

func (p *ChromePage) EnableDOM() error {
	return proto.DOMEnable{}.Call(p.page)
}

// --- Console ---

func (p *ChromePage) SetupConsoleListener(handler func(ConsoleMessage)) {
	go p.page.EachEvent(func(e *proto.RuntimeConsoleAPICalled) {
		var text strings.Builder
		for _, arg := range e.Args {
			s := arg.Value.String()
			if s != "" && s != "null" {
				text.WriteString(s + " ")
			}
		}

		level := "log"
		switch e.Type {
		case proto.RuntimeConsoleAPICalledTypeWarning:
			level = "warn"
		case proto.RuntimeConsoleAPICalledTypeError:
			level = "error"
		case proto.RuntimeConsoleAPICalledTypeInfo:
			level = "info"
		}

		handler(ConsoleMessage{
			Level: level,
			Text:  text.String(),
		})
	})()

	// Also listen for JS exceptions to capture errors
	go p.page.EachEvent(func(e *proto.RuntimeExceptionThrown) {
		if e.ExceptionDetails == nil {
			return
		}
		jsErr := &JSError{
			Text:   e.ExceptionDetails.Text,
			Line:   e.ExceptionDetails.LineNumber,
			Column: e.ExceptionDetails.ColumnNumber,
		}
		if e.ExceptionDetails.URL != "" {
			jsErr.URL = e.ExceptionDetails.URL
		}
		p.errMu.Lock()
		p.errors = append(p.errors, jsErr)
		if len(p.errors) > 500 {
			p.errors = p.errors[1:]
		}
		p.errMu.Unlock()
	})()
}

// --- Emulation ---

func (p *ChromePage) Emulate(opts EmulateOpts) error {
	if opts.UserAgent != "" {
		if err := (proto.NetworkSetUserAgentOverride{UserAgent: opts.UserAgent}).Call(p.page); err != nil {
			return fmt.Errorf("set user agent: %w", err)
		}
	}
	if opts.Width > 0 && opts.Height > 0 {
		scale := opts.Scale
		if scale <= 0 {
			scale = 1
		}
		orientation := &proto.EmulationScreenOrientation{
			Type:  proto.EmulationScreenOrientationTypePortraitPrimary,
			Angle: 0,
		}
		if opts.Landscape {
			orientation.Type = proto.EmulationScreenOrientationTypeLandscapePrimary
			orientation.Angle = 90
		}
		if err := (proto.EmulationSetDeviceMetricsOverride{
			Width:             opts.Width,
			Height:            opts.Height,
			DeviceScaleFactor: scale,
			Mobile:            opts.IsMobile,
			ScreenOrientation: orientation,
		}).Call(p.page); err != nil {
			return fmt.Errorf("set device metrics: %w", err)
		}
	}
	if opts.HasTouch {
		if err := (proto.EmulationSetTouchEmulationEnabled{
			Enabled: true,
		}).Call(p.page); err != nil {
			return fmt.Errorf("set touch emulation: %w", err)
		}
	}
	return nil
}

func (p *ChromePage) SetExtraHeaders(headers map[string]string) error {
	h := make(proto.NetworkHeaders)
	for k, v := range headers {
		h[k] = gson.New(v)
	}
	return proto.NetworkSetExtraHTTPHeaders{Headers: h}.Call(p.page)
}

func (p *ChromePage) SetOffline(offline bool) error {
	return proto.NetworkEmulateNetworkConditions{
		Offline: offline,
	}.Call(p.page)
}

// --- PDF ---

func (p *ChromePage) PDF(landscape bool) ([]byte, error) {
	reader, err := p.page.PDF(&proto.PagePrintToPDF{
		Landscape:       landscape,
		PrintBackground: true,
	})
	if err != nil {
		return nil, err
	}
	return io.ReadAll(reader)
}

// ---------------------------------------------------------------------------
// ChromeElement wraps *rod.Element to implement the Element interface.
// ---------------------------------------------------------------------------

type ChromeElement struct {
	el *rod.Element
}

func (e *ChromeElement) Click(button proto.InputMouseButton, clickCount int) error {
	return e.el.Click(button, clickCount)
}

func (e *ChromeElement) Hover() error {
	return e.el.Hover()
}

func (e *ChromeElement) Input(text string) error {
	return e.el.Input(text)
}
