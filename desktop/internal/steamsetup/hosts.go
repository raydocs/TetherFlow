// Package steamsetup owns only TetherFlow's explicitly marked Steam discovery entry.
package steamsetup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
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

const maxHostsBytes = 1024 * 1024

// faultHook is a test seam, nil in production. It is called with "after-journal",
// "mid-write", "after-write" and "after-sync" during the in-place write of Update.
var faultHook func(stage string)

func hook(stage string) {
	if faultHook != nil {
		faultHook(stage)
	}
}

// Update locks the existing file and writes in place, which keeps its identity and ACL.
// Under the lock it first recovers any write-ahead journal left by a crashed update, then
// journals old and new content before touching the file. A failed write is rolled back;
// if that fails too, or recovery is ambiguous, the journal is kept and the error matches
// ErrNeedsManualRecovery. No whole-file replacement or backup restores foreign data.
func Update(path, ip, lease string) (bool, error) {
	canon, err := canonicalHostsPath(path)
	if err != nil {
		return false, err
	}
	if err = checkDirTrusted(filepath.Dir(canon)); err != nil {
		return false, err
	}
	f, err := os.OpenFile(canon, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err = lockFile(f); err != nil {
		return false, err
	}
	defer unlockFile(f)
	cur, err := readLocked(f)
	if err != nil {
		return false, err
	}
	if cur, err = recoverJournals(f, canon, cur); err != nil {
		return false, err
	}
	old := cur
	next, err := Rewrite(old, ip, lease)
	if err != nil {
		return false, err
	}
	if bytes.Equal(old, next) {
		return false, nil
	}
	if len(next) > maxNewBytes {
		return false, errors.New("hosts update would exceed the size limit")
	}
	jp := journalPath(canon, lease)
	if err = writeJournal(jp, lease, old, next); err != nil {
		return false, err
	}
	hook("after-journal")
	if err = writeInPlace(f, next, true); err != nil {
		if rb := writeInPlace(f, old, false); rb != nil {
			return false, &ManualRecoveryError{Journal: jp, Reason: "write failed and rollback failed", Errs: []error{err, rb}}
		}
		_ = removeJournal(jp)
		return false, fmt.Errorf("hosts update: %w", err)
	}
	// A journal left behind here is recognised as applied by the next Update.
	_ = removeJournal(jp)
	return true, nil
}

func canonicalHostsPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func readLocked(f *os.File) ([]byte, error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > maxHostsBytes {
		return nil, errors.New("unexpected hosts file type or size")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxHostsBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxHostsBytes {
		return nil, errors.New("unexpected hosts file type or size")
	}
	return data, nil
}

// writeInPlace overwrites from offset 0, truncates and syncs. The hooks sit only on the
// main path so recovery and rollback never trigger a test crash.
func writeInPlace(f *os.File, data []byte, hooks bool) error {
	if _, e := f.Seek(0, io.SeekStart); e != nil {
		return e
	}
	h := len(data) / 2
	if _, e := f.Write(data[:h]); e != nil {
		return e
	}
	if hooks {
		hook("mid-write")
	}
	if _, e := f.Write(data[h:]); e != nil {
		return e
	}
	if hooks {
		hook("after-write")
	}
	if e := f.Truncate(int64(len(data))); e != nil {
		return e
	}
	e := f.Sync()
	if hooks {
		hook("after-sync")
	}
	return e
}

// Inspection describes a hosts file without changing anything.
type Inspection struct {
	Leases          []LeaseEntry
	ForeignOverride bool
	Malformed       bool
	PendingJournals []string
}

type LeaseEntry struct{ Lease, IP string }

// Inspect never writes and takes no lock, so it may race a running Update; callers treat it as a snapshot.
func Inspect(path string) (Inspection, error) {
	var in Inspection
	canon, err := canonicalHostsPath(path)
	if err != nil {
		return in, err
	}
	f, err := os.Open(canon)
	if err != nil {
		return in, err
	}
	defer f.Close()
	data, err := readLocked(f)
	if err != nil {
		return in, err
	}
	seen := map[string]bool{}
	lines := bytes.SplitAfter(data, []byte("\n"))
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(string(lines[i]))
		if strings.HasPrefix(line, begin) {
			l := strings.TrimPrefix(line, begin)
			j := i + 1
			for j < len(lines) && strings.TrimSpace(string(lines[j])) != end+l {
				j++
			}
			if !validLease.MatchString(l) || j == len(lines) || j-i-1 != 1 || seen[l] {
				in.Malformed = true
				if j == len(lines) {
					break
				}
				i = j
				continue
			}
			fields := strings.Fields(strings.TrimSpace(string(lines[i+1])))
			if len(fields) != 2 || net.ParseIP(fields[0]) == nil || fields[1] != Domain {
				in.Malformed = true
			} else {
				in.Leases = append(in.Leases, LeaseEntry{Lease: l, IP: fields[0]})
			}
			seen[l] = true
			i = j
			continue
		}
		f := strings.Fields(strings.SplitN(line, "#", 2)[0])
		for _, host := range f[minimum(1, len(f)):] {
			if strings.EqualFold(strings.TrimSuffix(host, "."), Domain) {
				in.ForeignOverride = true
			}
		}
	}
	names, err := journalNames(canon)
	if err != nil {
		return in, err
	}
	for _, n := range names {
		if n.tmp == "" {
			in.PendingJournals = append(in.PendingJournals, n.path)
		}
	}
	return in, nil
}
