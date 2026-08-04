//go:build linux

package doctor

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestProbeFanotifyIsReal runs the actual syscalls on this machine. It is the
// one check that decides whether checkpoint can work at all, so it must be
// exercised for real. When the privilege is missing, the test asserts the
// DIAGNOSIS is right and skips honestly rather than pretending to pass.
func TestProbeFanotifyIsReal(t *testing.T) {
	c := probeFanotify()
	if c.Name != "fanotify capability" || !c.Fatal {
		t.Fatalf("the capability check must be fatal: %+v", c)
	}
	if !c.OK {
		if c.Remedy == "" {
			t.Fatalf("an unavailable capability MUST come with a remedy: %+v", c)
		}
		for _, want := range []string{"setcap cap_sys_admin", "SYS_ADMIN", "root"} {
			if !strings.Contains(c.Remedy, want) {
				t.Errorf("remedy must mention %q; got %q", want, c.Remedy)
			}
		}
		t.Skipf("no fanotify privilege in this environment (diagnosis was: %s)", c.Detail)
	}
	if !strings.Contains(c.Detail, "armed") {
		t.Errorf("a successful probe must say it actually armed fanotify, got %q", c.Detail)
	}
	// The probe must clean up after itself: no leftover temp dirs from it.
	entries, err := os.ReadDir(os.TempDir())
	if err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "checkpoint-doctor-") {
				t.Errorf("probe left %s behind in %s", e.Name(), os.TempDir())
			}
		}
	}
}

// TestFanotifyFailureTranslatesErrnos pins the translation a stranger depends
// on: EPERM (fixable by capability) must never be confused with ENOSYS (kernel
// cannot do it at all), because the two have opposite remedies.
func TestFanotifyFailureTranslatesErrnos(t *testing.T) {
	dPerm, rPerm := fanotifyFailure("fanotify_init", unix.EPERM)
	if !strings.Contains(dPerm, "EPERM") || !strings.Contains(dPerm, "CAP_SYS_ADMIN") {
		t.Errorf("EPERM detail must name the errno and the capability, got %q", dPerm)
	}
	if !strings.Contains(rPerm, "setcap cap_sys_admin+ep") || !strings.Contains(rPerm, "--cap-add SYS_ADMIN") {
		t.Errorf("EPERM remedy must give the setcap and docker fixes, got %q", rPerm)
	}
	dSys, rSys := fanotifyFailure("fanotify_init", unix.ENOSYS)
	if !strings.Contains(dSys, "ENOSYS") || !strings.Contains(dSys, "CONFIG_FANOTIFY") {
		t.Errorf("ENOSYS detail must explain the kernel lacks fanotify, got %q", dSys)
	}
	if strings.Contains(rSys, "setcap") {
		t.Errorf("ENOSYS is not a permission problem; remedy must not suggest setcap: %q", rSys)
	}
	dOther, rOther := fanotifyFailure("fanotify_mark", unix.EINVAL)
	if !strings.Contains(dOther, "fanotify_mark") || rOther == "" {
		t.Errorf("unknown errnos must still name the call and give an action, got %q / %q", dOther, rOther)
	}
}

// TestProbeKernelReportsThisKernel: the line must contain the running release
// and the minimum, so a bug report carries both numbers.
func TestProbeKernelReportsThisKernel(t *testing.T) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		t.Skipf("uname unavailable: %v", err)
	}
	release := utsString(u.Release[:])
	c := probeKernel()
	if !strings.Contains(c.Detail, release) {
		t.Errorf("detail must name the running kernel %q, got %q", release, c.Detail)
	}
	if !strings.Contains(c.Detail, "5.1") {
		t.Errorf("detail must state the minimum supported version, got %q", c.Detail)
	}
	major, minor, ok := parseKernelVersion(release)
	if !ok {
		t.Skipf("cannot parse this kernel release %q", release)
	}
	wantOK := !older(major, minor, minKernelMajor, minKernelMinor)
	if c.OK != wantOK {
		t.Errorf("kernel %s: OK=%v, want %v", release, c.OK, wantOK)
	}
	if !c.OK && !c.Fatal {
		t.Error("an unsupported kernel must be fatal")
	}
}

func TestParseKernelVersion(t *testing.T) {
	cases := []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{"6.12.76-linuxkit", 6, 12, true},
		{"5.15.0-91-generic", 5, 15, true},
		{"5.1.0", 5, 1, true},
		{"4.19.0-21-amd64", 4, 19, true},
		{"6.8", 6, 8, true},
		{"6.6.0-rc1", 6, 6, true},
		{"5.10-custom", 5, 10, true},
		{"weird", 0, 0, false},
		{"", 0, 0, false},
		{"6", 0, 0, false},
	}
	for _, c := range cases {
		major, minor, ok := parseKernelVersion(c.in)
		if ok != c.ok || (ok && (major != c.major || minor != c.minor)) {
			t.Errorf("parseKernelVersion(%q) = %d,%d,%v; want %d,%d,%v",
				c.in, major, minor, ok, c.major, c.minor, c.ok)
		}
	}
}

func TestOlder(t *testing.T) {
	if !older(4, 19, 5, 1) || !older(5, 0, 5, 1) || older(5, 1, 5, 1) || older(6, 12, 5, 17) {
		t.Error("version comparison is wrong")
	}
}

// TestProbeFilesystemNamesTypeAndDegradesHonestly: on THIS machine (overlayfs
// in the devcontainer, ext4 elsewhere) the check must name the filesystem and,
// when the change feed is unavailable, say in words what stops working,
// without ever calling it fatal, because capture does not depend on it.
func TestProbeFilesystemNamesTypeAndDegradesHonestly(t *testing.T) {
	ws := t.TempDir()
	c := probeFilesystem(ws, true)
	if c.Fatal {
		t.Fatal("change-feed availability must never be fatal: capture works without it")
	}
	if c.Detail == "" {
		t.Fatal("filesystem check must always report what it found")
	}
	name, _, _, err := fsStat(ws)
	if err != nil {
		t.Fatalf("fsStat: %v", err)
	}
	if !strings.Contains(c.Detail, name) {
		t.Errorf("detail must name the filesystem type %q, got %q", name, c.Detail)
	}
	if !c.OK {
		if c.Remedy == "" {
			t.Error("a degraded filesystem must still come with guidance")
		}
		for _, want := range []string{"deletions are not attributed", "full"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("degrade detail must state the consequence %q, got %q", want, c.Detail)
			}
		}
	}
	t.Logf("filesystem check on this machine: OK=%v %s", c.OK, c.Detail)
}

// TestFsStatAndNames: statfs must produce a recognizable name and a nonzero
// capacity for a real directory, and unknown magics must be reported as hex
// rather than guessed.
func TestFsStatAndNames(t *testing.T) {
	name, free, total, err := fsStat(t.TempDir())
	if err != nil {
		t.Fatalf("fsStat: %v", err)
	}
	if name == "" || total == 0 {
		t.Errorf("fsStat returned name=%q total=%d", name, total)
	}
	if free > total {
		t.Errorf("free (%d) > total (%d)", free, total)
	}
	if _, _, _, err := fsStat("/definitely/not/a/path"); err == nil {
		t.Error("fsStat on a missing path must error, not report zeros as fact")
	}
	for magic, want := range map[uint64]string{
		0xEF53: "ext2/ext3/ext4", 0x58465342: "xfs", 0x9123683E: "btrfs",
		0x794c7630: "overlayfs", 0x01021994: "tmpfs",
	} {
		if got := fsName(magic); got != want {
			t.Errorf("fsName(%#x) = %q, want %q", magic, got, want)
		}
	}
	if got := fsName(0xdeadbeef); !strings.Contains(got, "0xdeadbeef") {
		t.Errorf("an unknown magic must be reported verbatim, got %q", got)
	}
}

// TestRunTextOnThisMachine renders the real report so a failure in CI carries
// the machine's actual diagnosis in the log (and proves Text never panics on
// real data).
func TestRunTextOnThisMachine(t *testing.T) {
	r := Run(t.TempDir(), t.TempDir()+"/store")
	out := r.Text()
	if len(strings.Split(strings.TrimRight(out, "\n"), "\n")) < len(r.Checks) {
		t.Errorf("Text dropped checks:\n%s", out)
	}
	t.Logf("checkpoint doctor on this machine (healthy=%v):\n%s", r.Healthy(), out)
}
