//go:build windows

package steamsetup

import (
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"
	"testing"
)

func sddl(t *testing.T, p string) string {
	t.Helper()
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "[System.IO.File]::GetAccessControl('"+strings.ReplaceAll(p, "'", "''")+"').GetSecurityDescriptorSddlForm([System.Security.AccessControl.AccessControlSections]::All)").CombinedOutput()
	if err != nil {
		t.Fatalf("Get-Acl: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestUpdatePreservesExplicitDACL(t *testing.T) {
	_, p := writeHosts(t, crashBase)
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", p, "/inheritance:r", "/grant:r", me.Username+":(F)", "/grant:r", "*S-1-5-32-545:(R)").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v\n%s", err, out)
	}
	before := sddl(t, p)
	for _, ip := range []string{"192.168.1.5", ""} {
		if _, err := Update(p, ip, lease); err != nil {
			t.Fatal(err)
		}
		if after := sddl(t, p); after != before {
			t.Fatalf("DACL changed\nbefore %s\nafter  %s", before, after)
		}
	}
}

func TestForeignWriterBlockedDuringUpdate(t *testing.T) {
	_, p := writeHosts(t, crashBase)
	var werr error
	faultHook = func(stage string) {
		if stage != "mid-write" {
			return
		}
		g, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			werr = err
			return
		}
		defer g.Close()
		_, werr = g.WriteAt([]byte("#"), 0)
	}
	defer func() { faultHook = nil }()
	if _, err := Update(p, "192.168.1.5", lease); err != nil {
		t.Fatal(err)
	}
	var errno syscall.Errno
	if !asErrno(werr, &errno) || errno != 33 {
		t.Fatalf("foreign write during update: want errno 33, got %v", werr)
	}
}

func asErrno(err error, out *syscall.Errno) bool {
	for err != nil {
		if e, ok := err.(syscall.Errno); ok {
			*out = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
