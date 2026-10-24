package steamsetup

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const lease = "0123456789abcdef0123456789abcdef"

func TestRoundTripPreservesForeignBytes(t *testing.T) {
	for _, base := range []string{"", "# custom\n127.0.0.1 localhost\n", "# custom\r\n127.0.0.1 localhost\r\n"} {
		t.Run(base, func(t *testing.T) {
			added, e := Rewrite([]byte(base), "192.168.1.5", lease)
			if e != nil {
				t.Fatal(e)
			}
			again, e := Rewrite(added, "192.168.1.5", lease)
			if e != nil || !bytes.Equal(added, again) {
				t.Fatalf("not idempotent %v", e)
			}
			restored, e := Rewrite(added, "", lease)
			if e != nil || string(restored) != base {
				t.Fatalf("roundtrip %q %v", restored, e)
			}
			restored, e = Rewrite(restored, "", lease)
			if e != nil || string(restored) != base {
				t.Fatal("cleanup not idempotent")
			}
		})
	}
}
func TestForeignAppendsSurvive(t *testing.T) {
	data, _ := Rewrite([]byte("# before\n"), "192.168.1.5", lease)
	data = append(data, []byte("10.0.0.1 other.local\n")...)
	out, e := Rewrite(data, "", lease)
	if e != nil || string(out) != "# before\n10.0.0.1 other.local\n" {
		t.Fatalf("%q %v", out, e)
	}
}
func TestRefusesForeignOrMalformed(t *testing.T) {
	for _, data := range []string{"1.2.3.4 lancache.steamcontent.com\n", "1.2.3.4 LANCACHE.STEAMCONTENT.COM. # existing\n", begin + strings.Repeat("a", 32) + "\n", begin + lease + "\n", begin + lease + "\n1.2.3.4 foreign\n" + end + lease + "\n", "# no newline"} {
		if _, e := Rewrite([]byte(data), "192.168.1.5", lease); e == nil {
			t.Fatalf("accepted %q", data)
		}
	}
	for _, ip := range []string{"127.0.0.1", "8.8.8.8", "::1", "invalid"} {
		if _, e := Rewrite(nil, ip, lease); e == nil {
			t.Fatal(ip)
		}
	}
	if _, e := Rewrite(nil, "192.168.1.5", "bad\n"); e == nil {
		t.Fatal("lease injection")
	}
}
func TestUpdatePreservesModeAndChanges(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts")
	base := []byte("# original\n")
	if e := os.WriteFile(p, base, 0600); e != nil {
		t.Fatal(e)
	}
	if changed, e := Update(p, "192.168.1.5", lease); e != nil || !changed {
		t.Fatal(changed, e)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("# foreign\n")
	_ = f.Close()
	if _, e := Update(p, "", lease); e != nil {
		t.Fatal(e)
	}
	out, _ := os.ReadFile(p)
	if string(out) != "# original\n# foreign\n" {
		t.Fatal(string(out))
	}
	st, _ := os.Stat(p)
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}
func TestConcurrentLeasesDoNotOverwrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts")
	_ = os.WriteFile(p, []byte("# base\n"), 0600)
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for _, l := range []string{lease, strings.Repeat("a", 32)} {
		wg.Add(1)
		go func(l string) {
			defer wg.Done()
			if _, e := Update(p, "192.168.1.5", l); e == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(l)
	}
	wg.Wait()
	if success != 1 {
		t.Fatal(success)
	}
}
