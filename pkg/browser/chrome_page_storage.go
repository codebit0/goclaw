package browser

import (
	"fmt"
	"net/url"

	"github.com/go-rod/rod/lib/proto"
)

// --- Chrome extensions: Cookies ---

func (p *ChromePage) GetCookies() ([]*Cookie, error) {
	result, err := proto.NetworkGetCookies{}.Call(p.page)
	if err != nil {
		return nil, err
	}
	cookies := make([]*Cookie, len(result.Cookies))
	for i, c := range result.Cookies {
		cookies[i] = &Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.Secure,
			HTTPOnly: c.HTTPOnly,
			SameSite: string(c.SameSite),
			Expires:  float64(c.Expires),
		}
	}
	return cookies, nil
}

func (p *ChromePage) SetCookie(c *Cookie) error {
	params := proto.NetworkSetCookie{
		Name:     c.Name,
		Value:    c.Value,
		Domain:   c.Domain,
		Path:     c.Path,
		Secure:   c.Secure,
		HTTPOnly: c.HTTPOnly,
	}
	if c.URL != "" {
		params.URL = c.URL
	}
	if c.Expires > 0 {
		params.Expires = proto.TimeSinceEpoch(c.Expires)
	}
	if c.SameSite != "" {
		params.SameSite = proto.NetworkCookieSameSite(c.SameSite)
	}
	_, err := params.Call(p.page)
	return err
}

func (p *ChromePage) ClearCookies() error {
	return proto.NetworkClearBrowserCookies{}.Call(p.page)
}

// --- Chrome extensions: DOM Storage ---

func (p *ChromePage) GetStorageItems(isLocal bool) (map[string]string, error) {
	sid, err := p.storageID(isLocal)
	if err != nil {
		return nil, err
	}
	_ = proto.DOMStorageEnable{}.Call(p.page)
	result, err := proto.DOMStorageGetDOMStorageItems{StorageID: sid}.Call(p.page)
	if err != nil {
		return nil, err
	}
	items := make(map[string]string, len(result.Entries))
	for _, entry := range result.Entries {
		if len(entry) >= 2 {
			items[entry[0]] = entry[1]
		}
	}
	return items, nil
}

func (p *ChromePage) SetStorageItem(isLocal bool, key, value string) error {
	sid, err := p.storageID(isLocal)
	if err != nil {
		return err
	}
	_ = proto.DOMStorageEnable{}.Call(p.page)
	return proto.DOMStorageSetDOMStorageItem{
		StorageID: sid,
		Key:       key,
		Value:     value,
	}.Call(p.page)
}

func (p *ChromePage) ClearStorage(isLocal bool) error {
	sid, err := p.storageID(isLocal)
	if err != nil {
		return err
	}
	_ = proto.DOMStorageEnable{}.Call(p.page)
	return proto.DOMStorageClear{StorageID: sid}.Call(p.page)
}

// storageID builds a DOMStorageStorageID from the page's security origin.
func (p *ChromePage) storageID(isLocal bool) (*proto.DOMStorageStorageID, error) {
	info, err := p.page.Info()
	if err != nil || info == nil {
		return nil, fmt.Errorf("get page info for storage: %w", err)
	}
	parsed, err := url.Parse(info.URL)
	if err != nil {
		return nil, fmt.Errorf("parse page URL: %w", err)
	}
	origin := fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
	return &proto.DOMStorageStorageID{
		SecurityOrigin: origin,
		IsLocalStorage: isLocal,
	}, nil
}

// --- Chrome extensions: JS Errors ---

func (p *ChromePage) GetJSErrors() ([]*JSError, error) {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	result := make([]*JSError, len(p.errors))
	copy(result, p.errors)
	p.errors = nil
	return result, nil
}
