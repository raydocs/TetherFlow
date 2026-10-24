package usbmux

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	maxPacket    = 1 << 20
	maxDepth     = 32
	maxNodes     = 32768
	maxString    = 64 << 10
	appleDoctype = `DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"`
)

type plistParser struct {
	d                            *xml.Decoder
	nodes                        int
	root, directive, declaration bool
}

func protocol(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...))
}

// Only the fixed Apple declaration is inertly accepted. encoding/xml is never
// given an entity resolver or a non-default Entity map.
func (p *plistParser) token() (xml.Token, error) {
	for {
		t, err := p.d.Token()
		if err != nil {
			return nil, err
		}
		p.nodes++
		if p.nodes > maxNodes {
			return nil, protocol("too many XML nodes")
		}
		switch v := t.(type) {
		case xml.Directive:
			if p.root || p.directive || strings.Join(strings.Fields(string(v)), " ") != appleDoctype {
				return nil, protocol("unsupported XML directive")
			}
			p.directive = true
			continue
		case xml.ProcInst:
			if p.root || p.declaration || p.directive || v.Target != "xml" || len(v.Inst) > maxString {
				return nil, protocol("unsupported XML instruction")
			}
			p.declaration = true
			continue
		case xml.Comment:
			if len(v) > maxString {
				return nil, protocol("XML comment too long")
			}
			continue
		case xml.CharData:
			if len(v) > maxString {
				return nil, protocol("XML text too long")
			}
		}
		return t, nil
	}
}
func (p *plistParser) nonspace() (xml.Token, error) {
	for {
		t, e := p.token()
		if e != nil {
			return nil, e
		}
		if c, ok := t.(xml.CharData); ok && strings.TrimSpace(string(c)) == "" {
			continue
		}
		return t, nil
	}
}
func plain(s xml.StartElement) bool { return s.Name.Space == "" && len(s.Attr) == 0 }
func (p *plistParser) text(s xml.StartElement) (string, error) {
	var b strings.Builder
	for {
		t, e := p.token()
		if e != nil {
			return "", e
		}
		switch v := t.(type) {
		case xml.CharData:
			if b.Len()+len(v) > maxString {
				return "", protocol("string too long")
			}
			b.Write(v)
		case xml.EndElement:
			if v.Name != s.Name {
				return "", protocol("mismatched XML element")
			}
			return b.String(), nil
		default:
			return "", protocol("nested scalar")
		}
	}
}
func (p *plistParser) value(s xml.StartElement, depth int) (any, error) {
	if depth > maxDepth || !plain(s) {
		return nil, protocol("invalid plist depth/attributes")
	}
	switch s.Name.Local {
	case "dict":
		m := map[string]any{}
		for {
			t, e := p.nonspace()
			if e != nil {
				return nil, e
			}
			if end, ok := t.(xml.EndElement); ok {
				if end.Name != s.Name {
					return nil, protocol("dict ending")
				}
				return m, nil
			}
			key, ok := t.(xml.StartElement)
			if !ok || key.Name.Local != "key" || !plain(key) {
				return nil, protocol("dict key expected")
			}
			k, e := p.text(key)
			if e != nil {
				return nil, e
			}
			if _, exists := m[k]; exists {
				return nil, protocol("duplicate key %q", k)
			}
			t, e = p.nonspace()
			if e != nil {
				return nil, e
			}
			start, ok := t.(xml.StartElement)
			if !ok {
				return nil, protocol("dict value expected")
			}
			v, e := p.value(start, depth+1)
			if e != nil {
				return nil, e
			}
			m[k] = v
		}
	case "array":
		a := []any{}
		for {
			t, e := p.nonspace()
			if e != nil {
				return nil, e
			}
			if end, ok := t.(xml.EndElement); ok {
				if end.Name != s.Name {
					return nil, protocol("array ending")
				}
				return a, nil
			}
			start, ok := t.(xml.StartElement)
			if !ok {
				return nil, protocol("array value expected")
			}
			v, e := p.value(start, depth+1)
			if e != nil {
				return nil, e
			}
			a = append(a, v)
		}
	case "string", "integer", "true", "false", "data", "real", "date":
		text, e := p.text(s)
		if e != nil {
			return nil, e
		}
		switch s.Name.Local {
		case "string":
			return text, nil
		case "integer":
			n, e := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
			if e != nil {
				return nil, protocol("invalid integer")
			}
			return n, nil
		case "true", "false":
			if strings.TrimSpace(text) != "" {
				return nil, protocol("nonempty boolean")
			}
			return s.Name.Local == "true", nil
		case "data":
			b, e := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text), ""))
			if e != nil {
				return nil, protocol("invalid data")
			}
			return b, nil
		case "real":
			n, e := strconv.ParseFloat(strings.TrimSpace(text), 64)
			if e != nil {
				return nil, protocol("invalid real")
			}
			return n, nil
		case "date":
			n, e := time.Parse(time.RFC3339, strings.TrimSpace(text))
			if e != nil {
				return nil, protocol("invalid date")
			}
			return n, nil
		}
	}
	return nil, protocol("unsupported plist element %q", s.Name.Local)
}
func parsePlist(b []byte) (map[string]any, error) {
	if len(b) > maxPacket-16 {
		return nil, protocol("plist too large")
	}
	p := plistParser{d: xml.NewDecoder(bytes.NewReader(b))}
	t, e := p.nonspace()
	if e != nil {
		return nil, protocol("XML: %v", e)
	}
	s, ok := t.(xml.StartElement)
	if !ok || s.Name.Space != "" || s.Name.Local != "plist" || len(s.Attr) != 1 || s.Attr[0].Name.Space != "" || s.Attr[0].Name.Local != "version" || s.Attr[0].Value != "1.0" {
		return nil, protocol("plist root expected")
	}
	p.root = true
	t, e = p.nonspace()
	if e != nil {
		return nil, protocol("XML: %v", e)
	}
	s, ok = t.(xml.StartElement)
	if !ok {
		return nil, protocol("plist dictionary expected")
	}
	v, e := p.value(s, 1)
	if e != nil {
		return nil, protocol("XML: %v", e)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, protocol("plist dictionary expected")
	}
	t, e = p.nonspace()
	if e != nil {
		return nil, protocol("XML: %v", e)
	}
	end, ok := t.(xml.EndElement)
	if !ok || end.Name.Space != "" || end.Name.Local != "plist" {
		return nil, protocol("plist ending expected")
	}
	if _, e = p.nonspace(); e != io.EOF {
		return nil, protocol("trailing XML: %v", e)
	}
	return m, nil
}
