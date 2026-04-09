package browser

import (
	"context"

	"github.com/go-rod/rod/lib/proto"
)

// StartScreencast begins streaming JPEG frames to ch. Multiple callers may subscribe;
// CDP screencast is started only once and frames fan-out to all subscribers.
func (p *ChromePage) StartScreencast(fps int, quality int, maxWidth int, maxHeight int, ch chan<- ScreencastFrame) error {
	if fps <= 0 {
		fps = 10
	}
	if quality <= 0 {
		quality = 80
	}
	if maxWidth <= 0 {
		maxWidth = 1280
	}
	if maxHeight <= 0 {
		maxHeight = 720
	}

	p.screencastMu.Lock()
	defer p.screencastMu.Unlock()

	// Add subscriber to the fan-out set.
	if p.screencastSubs == nil {
		p.screencastSubs = make(map[chan<- ScreencastFrame]struct{})
	}
	p.screencastSubs[ch] = struct{}{}

	// If CDP screencast + event listener are already active, just add the subscriber.
	if p.screencastOn {
		return nil
	}

	err := proto.PageStartScreencast{
		Format:    proto.PageStartScreencastFormatJpeg,
		Quality:   &quality,
		MaxWidth:  &maxWidth,
		MaxHeight: &maxHeight,
	}.Call(p.page)
	if err != nil {
		return err
	}

	p.screencastOn = true

	// Create a cancellable page context so StopScreencast can kill the listener goroutine.
	ctx, cancel := context.WithCancel(p.page.GetContext())
	p.screencastStop = cancel
	scPage := p.page.Context(ctx)

	go scPage.EachEvent(func(e *proto.PageScreencastFrame) {
		frame := ScreencastFrame{
			Data:      e.Data,
			SessionID: int(e.SessionID),
			Metadata: ScreencastMetadata{
				OffsetTop:       e.Metadata.OffsetTop,
				PageScaleFactor: e.Metadata.PageScaleFactor,
				DeviceWidth:     e.Metadata.DeviceWidth,
				DeviceHeight:    e.Metadata.DeviceHeight,
				ScrollOffsetX:   e.Metadata.ScrollOffsetX,
				ScrollOffsetY:   e.Metadata.ScrollOffsetY,
				Timestamp:       float64(e.Metadata.Timestamp),
			},
		}
		_ = proto.PageScreencastFrameAck{SessionID: e.SessionID}.Call(p.page)

		p.screencastMu.Lock()
		subs := make([]chan<- ScreencastFrame, 0, len(p.screencastSubs))
		for s := range p.screencastSubs {
			subs = append(subs, s)
		}
		p.screencastMu.Unlock()

		// Fan-out: non-blocking send to all subscribers
		for _, dest := range subs {
			select {
			case dest <- frame:
			default:
			}
		}
	})()

	return nil
}

// StopScreencast removes all subscribers and stops CDP screencast.
func (p *ChromePage) StopScreencast() error {
	p.screencastMu.Lock()
	p.screencastSubs = nil
	wasOn := p.screencastOn
	p.screencastOn = false
	cancel := p.screencastStop
	p.screencastStop = nil
	p.screencastMu.Unlock()

	if wasOn {
		_ = proto.PageStopScreencast{}.Call(p.page)
		if cancel != nil {
			cancel()
		}
	}
	return nil
}

// StopScreencastCh removes a single subscriber. CDP screencast is stopped only when
// the last subscriber is removed.
func (p *ChromePage) StopScreencastCh(ch chan<- ScreencastFrame) {
	p.screencastMu.Lock()
	delete(p.screencastSubs, ch)
	remaining := len(p.screencastSubs)
	p.screencastMu.Unlock()

	if remaining == 0 {
		// Last subscriber gone — fully stop CDP screencast
		_ = p.StopScreencast()
	}
}
