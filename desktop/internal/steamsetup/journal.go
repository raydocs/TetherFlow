package steamsetup

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNeedsManualRecovery marks a state this package refuses to guess about: the journal is
// kept and every later Update fails until a person resolves it.
var ErrNeedsManualRecovery = errors.New("hosts file needs manual recovery")

// ManualRecoveryError names the journal that blocks updates.
type ManualRecoveryError struct {
	Journal string
	Reason  string
	Errs    []error
}

func (e *ManualRecoveryError) Error() string {
	s := fmt.Sprintf("%v: %s (journal %s)", ErrNeedsManualRecovery, e.Reason, e.Journal)
	for _, x := range e.Errs {
		s += ": " + x.Error()
	}
	return s
}
func (e *ManualRecoveryError) Unwrap() []error {
	return append([]error{ErrNeedsManualRecovery}, e.Errs...)
}

// Journal layout: magic | lease(32) | len(old) u32 | len(new) u32 | sha256(old) | sha256(new) | old | new | sha256(all before).
const (
	journalMagic  = "TFHJRNL1"
	journalHeader = 8 + 32 + 4 + 4 + 32 + 32
	maxNewBytes   = maxHostsBytes + 64*1024
	maxJournal    = journalHeader + maxHostsBytes + maxNewBytes + 32
	journalMid    = ".tetherflow-"
	journalSuffix = ".journal"
	tmpSuffix     = ".tmp"
)

func journalPath(canon, lease string) string {
	return canon + journalMid + lease + journalSuffix
}

func encodeJournal(lease string, old, next []byte) []byte {
	var b bytes.Buffer
	b.WriteString(journalMagic)
	b.WriteString(lease)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(old)))
	b.Write(n[:])
	binary.BigEndian.PutUint32(n[:], uint32(len(next)))
	b.Write(n[:])
	so, sn := sha256.Sum256(old), sha256.Sum256(next)
	b.Write(so[:])
	b.Write(sn[:])
	b.Write(old)
	b.Write(next)
	sum := sha256.Sum256(b.Bytes())
	b.Write(sum[:])
	return b.Bytes()
}

func decodeJournal(data []byte, lease string) (old, next []byte, err error) {
	bad := errors.New("journal is corrupt")
	if len(data) < journalHeader+32 || string(data[:8]) != journalMagic || string(data[8:40]) != lease {
		return nil, nil, bad
	}
	body, tail := data[:len(data)-32], data[len(data)-32:]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], tail) {
		return nil, nil, bad
	}
	lo, ln := int(binary.BigEndian.Uint32(data[40:44])), int(binary.BigEndian.Uint32(data[44:48]))
	if lo > maxHostsBytes || ln > maxNewBytes || journalHeader+lo+ln+32 != len(data) {
		return nil, nil, bad
	}
	old, next = data[journalHeader:journalHeader+lo], data[journalHeader+lo:journalHeader+lo+ln]
	so, sn := sha256.Sum256(old), sha256.Sum256(next)
	if !bytes.Equal(so[:], data[48:80]) || !bytes.Equal(sn[:], data[80:112]) {
		return nil, nil, bad
	}
	return old, next, nil
}

// writeJournal publishes the journal atomically: a torn temp file is never a journal.
func writeJournal(jp, lease string, old, next []byte) error {
	tmp := jp + tmpSuffix
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(encodeJournal(lease, old, next))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, jp)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write hosts journal: %w", err)
	}
	syncDir(filepath.Dir(jp))
	return nil
}

func removeJournal(jp string) error {
	err := os.Remove(jp)
	if err == nil {
		syncDir(filepath.Dir(jp))
	}
	return err
}

// readJournalFile refuses links and non-regular files and never reads more than maxJournal bytes.
func readJournalFile(p string) ([]byte, error) {
	lst, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !lst.Mode().IsRegular() || lst.Size() > maxJournal {
		return nil, errors.New("journal is not a regular file of bounded size")
	}
	f, err := os.OpenFile(p, os.O_RDONLY|noFollowFlag, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(lst, st) || !st.Mode().IsRegular() || st.Size() > maxJournal {
		return nil, errors.New("journal changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxJournal+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxJournal {
		return nil, errors.New("journal exceeds size limit")
	}
	return data, nil
}

type journalName struct{ path, lease, tmp string }

// journalNames lists journals and torn temp files next to canon, ignoring anything whose
// name is not exactly <hosts>.tetherflow-<32 hex>.journal[.tmp].
func journalNames(canon string) ([]journalName, error) {
	entries, err := os.ReadDir(filepath.Dir(canon))
	if err != nil {
		return nil, err
	}
	prefix := filepath.Base(canon) + journalMid
	var out []journalName
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		rest, tmp := strings.TrimPrefix(n, prefix), ""
		if strings.HasSuffix(rest, journalSuffix+tmpSuffix) {
			rest, tmp = strings.TrimSuffix(rest, journalSuffix+tmpSuffix), n
		} else if strings.HasSuffix(rest, journalSuffix) {
			rest = strings.TrimSuffix(rest, journalSuffix)
		} else {
			continue
		}
		if !validLease.MatchString(rest) {
			continue
		}
		out = append(out, journalName{path: filepath.Join(filepath.Dir(canon), n), lease: rest, tmp: tmp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

type journalState int

const (
	stateAmbiguous  journalState = iota
	stateOld                     // update was not applied
	stateNew                     // update was applied
	stateNewForeign              // applied, foreign text appended since
	stateTorn                    // new[:k] + old[k:]: finish forward
)

func classify(cur, old, next []byte) journalState {
	switch {
	case bytes.Equal(cur, old):
		return stateOld
	case bytes.Equal(cur, next):
		return stateNew
	case isTorn(cur, old, next):
		return stateTorn
	case bytes.HasPrefix(cur, next):
		return stateNewForeign
	}
	return stateAmbiguous
}

// isTorn reports cur == next[:k] + old[min(k,len(old)):] for some 0 <= k <= len(next), the only
// shapes an in-place front-to-back overwrite can leave before the truncate.
func isTorn(cur, old, next []byte) bool {
	if len(cur) > len(old) {
		return len(cur) <= len(next) && bytes.Equal(cur, next[:len(cur)])
	}
	if len(cur) != len(old) {
		return false
	}
	p := 0
	for p < len(cur) && p < len(next) && cur[p] == next[p] {
		p++
	}
	q := len(cur)
	for q > 0 && cur[q-1] == old[q-1] {
		q--
	}
	return q <= p
}

// recoverJournals runs under the hosts lock. It returns the file content after recovery.
func recoverJournals(f *os.File, canon string, cur []byte) ([]byte, error) {
	names, err := journalNames(canon)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if n.tmp != "" {
			if err := os.Remove(n.path); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			continue
		}
		manual := func(reason string, errs ...error) error {
			return &ManualRecoveryError{Journal: n.path, Reason: reason, Errs: errs}
		}
		data, err := readJournalFile(n.path)
		if err != nil {
			return nil, manual("journal unreadable", err)
		}
		old, next, err := decodeJournal(data, n.lease)
		if err != nil {
			return nil, manual("journal invalid", err)
		}
		switch classify(cur, old, next) {
		case stateAmbiguous:
			return nil, manual("hosts file changed after an interrupted update; inspect it and remove the journal")
		case stateTorn:
			if err := writeInPlace(f, next, false); err != nil {
				return nil, manual("could not finish an interrupted update", err)
			}
			cur = append([]byte(nil), next...)
		}
		if err := removeJournal(n.path); err != nil {
			return nil, manual("could not remove a resolved journal", err)
		}
	}
	return cur, nil
}
