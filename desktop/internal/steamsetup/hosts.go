// Package steamsetup owns only TetherFlow's explicitly marked Steam discovery entry.
package steamsetup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
)

const Domain = "lancache.steamcontent.com"
const begin = "# BEGIN TetherFlow Steam "
const end = "# END TetherFlow Steam "

var validLease = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Rewrite preserves every unrelated byte. Existing discovery overrides are never overwritten.
// An empty ip removes only the exact matching lease, making guardian cleanup idempotent.
func Rewrite(data []byte, ip, lease string) ([]byte, error) {
	if !validLease.MatchString(lease) {
		return nil, errors.New("invalid session lease")
	}
	startMark, endMark := begin+lease, end+lease
	lines := bytes.SplitAfter(data, []byte("\n"))
	var out bytes.Buffer
	removed := false
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(string(lines[i]))
		if strings.HasPrefix(line, begin) {
			if line != startMark {
				return nil, errors.New("another TetherFlow session owns the Steam hosts entry; recover explicitly")
			}
			j := i + 1
			for j < len(lines) && strings.TrimSpace(string(lines[j])) != endMark {
				j++
			}
			if j == len(lines) {
				return nil, errors.New("incomplete TetherFlow hosts block; manual inspection required")
			}
			// Never delete a foreign or modified hosts record hidden inside our block.
			content := lines[i+1 : j]
			if len(content) != 1 {
				return nil, errors.New("modified TetherFlow hosts block; refusing removal")
			}
			fields := strings.Fields(strings.TrimSpace(string(content[0])))
			if len(fields) != 2 || net.ParseIP(fields[0]) == nil || fields[1] != Domain {
				return nil, errors.New("modified TetherFlow hosts entry; refusing removal")
			}
			if removed {
				return nil, errors.New("duplicate TetherFlow hosts block")
			}
			removed = true
			i = j
			continue
		}
		if ip != "" {
			record := strings.SplitN(line, "#", 2)[0]
			f := strings.Fields(record)
			for _, host := range f[minimum(1, len(f)):] {
				if strings.EqualFold(strings.TrimSuffix(host, "."), Domain) {
					return nil, errors.New("existing Steam discovery override belongs to another tool; refusing overwrite")
				}
			}
		}
		out.Write(lines[i])
	}
	if ip == "" {
		return out.Bytes(), nil
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil || !parsed.IsPrivate() {
		return nil, errors.New("Steam discovery requires an RFC1918 IPv4 address")
	}
	// Requiring a final newline avoids silently changing the user's pre-existing file.
	if out.Len() > 0 && out.Bytes()[out.Len()-1] != '\n' {
		return nil, errors.New("hosts file has no final newline; add one explicitly before enabling")
	}
	newline := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		newline = "\r\n"
	}
	fmt.Fprintf(&out, "%s%s%s %s%s%s%s", startMark, newline, parsed.String(), Domain, newline, endMark, newline)
	return out.Bytes(), nil
}
func minimum(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Update locks the existing file and writes in-place to preserve Windows hosts ACLs.
// A failed write attempts rollback; no whole-file replacement or backup restores foreign data.
func Update(path, ip, lease string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err = lockFile(f); err != nil {
		return false, err
	}
	defer unlockFile(f)
	stat, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 1024*1024 {
		return false, errors.New("unexpected hosts file type or size")
	}
	old, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		return false, err
	}
	next, err := Rewrite(old, ip, lease)
	if err != nil {
		return false, err
	}
	if bytes.Equal(old, next) {
		return false, nil
	}
	write := func(data []byte) error {
		if _, e := f.Seek(0, 0); e != nil {
			return e
		}
		if _, e := f.Write(data); e != nil {
			return e
		}
		if e := f.Truncate(int64(len(data))); e != nil {
			return e
		}
		return f.Sync()
	}
	if err = write(next); err != nil {
		rollback := write(old)
		return false, fmt.Errorf("hosts update: %w (rollback: %v)", err, rollback)
	}
	return true, nil
}
