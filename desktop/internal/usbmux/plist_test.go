package usbmux

import (
	"errors"
	"strings"
	"testing"
)

func plist(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n<!" + appleDoctype + ">\n" + `<plist version="1.0">` + body + `</plist>`
}
func TestPlistCanonicalAndBounds(t *testing.T) {
	cases := []struct {
		name, xml string
		valid     bool
	}{
		{"canonical", plist(`<dict><key>DeviceList</key><array/></dict>`), true},
		{"no doctype", `<plist version="1.0"><dict/></plist>`, true},
		{"escaped text", plist(`<dict><key>x</key><string>&amp;&lt;&#65;</string></dict>`), true},
		{"internal subset", `<!DOCTYPE plist [<!ENTITY x "hello">]><plist version="1.0"><dict/></plist>`, false},
		{"external entity", `<!DOCTYPE plist [<!ENTITY x SYSTEM "file:///etc/passwd">]><plist version="1.0"><dict><key>x</key><string>&x;</string></dict></plist>`, false},
		{"unknown external dtd", `<!DOCTYPE plist SYSTEM "https://example.invalid/x"><plist version="1.0"><dict/></plist>`, false},
		{"unknown directive", `<!FOO bar><plist version="1.0"><dict/></plist>`, false},
		{"second doctype", "<!" + appleDoctype + "><!" + appleDoctype + `><plist version="1.0"><dict/></plist>`, false},
		{"inside doctype", `<plist version="1.0"><!` + appleDoctype + `><dict/></plist>`, false},
		{"entity outside subset", `<!ENTITY x "value"><plist version="1.0"><dict/></plist>`, false},
		{"unknown entity", plist(`<dict><key>x</key><string>&bogus;</string></dict>`), false},
		{"duplicate keys", plist(`<dict><key>x</key><integer>1</integer><key>x</key><integer>2</integer></dict>`), false},
		{"namespace", `<plist xmlns="http://bad" version="1.0"><dict/></plist>`, false},
		{"scalar child", plist(`<dict><key>x</key><string><string>x</string></string></dict>`), false},
		{"missing value", plist(`<dict><key>x</key></dict>`), false},
		{"bad integer", plist(`<dict><key>x</key><integer>18446744073709551616</integer></dict>`), false},
		{"bad scalar attr", plist(`<dict><key>x</key><string a="x">x</string></dict>`), false},
		{"wrong top type", plist(`<array/>`), false},
		{"trailing root", plist(`<dict/>`) + `<dict/>`, false},
		{"non XML instruction", `<?unsafe test?>` + plist(`<dict/>`), false},
		{"depth", plist(`<dict><key>x</key>` + strings.Repeat(`<array>`, maxDepth) + strings.Repeat(`</array>`, maxDepth) + `</dict>`), false},
		{"nodes", plist(`<dict><key>x</key><array>` + strings.Repeat(`<true/>`, maxNodes) + `</array></dict>`), false},
		{"long string", plist(`<dict><key>x</key><string>` + strings.Repeat("a", maxString+1) + `</string></dict>`), false},
		{"malformed", plist(`<dict>`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, e := parsePlist([]byte(tc.xml))
			if (e == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, e)
			}
			if e != nil && !errors.Is(e, ErrProtocol) {
				t.Fatalf("not protocol error: %v", e)
			}
		})
	}
}
func FuzzPlist(f *testing.F) {
	f.Add([]byte(plist(`<dict/>`)))
	f.Add([]byte(plist(`<dict><key>DeviceList</key><array/></dict>`)))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = parsePlist(b) })
}
