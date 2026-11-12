package containerenv

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lima-vm/sshocker/pkg/reversesshfs"
)

func TestShareFailureDoesNotExitNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	started := make(chan struct{}, 2)
	finished := make(chan error, 1)
	go func() {
		finished <- retryUntilCancelled(ctx, func(context.Context) error {
			attempts++
			started <- struct{}{}
			if attempts == 1 {
				return io.EOF
			}
			<-ctx.Done()
			return ctx.Err()
		}, nil)
	}()
	<-started
	select {
	case err := <-finished:
		t.Fatalf("share EOF exited the supervisor: %v", err)
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("share EOF was not retried")
	}
	tap := make(chan error)
	ssh := make(chan error)
	networkFinished := make(chan error, 1)
	go func() { networkFinished <- waitNetworkExit(ctx, tap, ssh) }()
	select {
	case err := <-networkFinished:
		t.Fatalf("network exited while the share was reconnecting: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled share retry did not stop")
	}
	select {
	case err := <-networkFinished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("network wait ignored cancellation")
	}
}

func TestBlockedShareWaitsInsteadOfExiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- retryUntilCancelled(ctx, func(context.Context) error { return errShareBlocked }, nil)
	}()
	select {
	case err := <-finished:
		t.Fatalf("blocked share ended the worker: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked share ignored cancellation")
	}
}

func TestShareCleanupRequiresMountIdentity(t *testing.T) {
	mount := "/cage/workspace"
	ours := shareIdentity{Mountpoint: mount, Source: ":" + mount, FSType: "fuse.sshfs", MountDevice: "7:9"}
	if err := decideSharePrepare(true, ours, nil); err == nil {
		t.Fatal("path alone authorized an unmount")
	}
	foreign := ours
	foreign.FSType = "ext4"
	if err := decideSharePrepare(true, foreign, &ours); !errors.Is(err, errShareBlocked) {
		t.Fatal("foreign filesystem was accepted", err)
	}
	replaced := ours
	replaced.MountDevice = "8:1"
	if err := decideSharePrepare(true, replaced, &ours); !errors.Is(err, errShareBlocked) {
		t.Fatal("replaced mount device was accepted", err)
	}
	if err := decideSharePrepare(true, ours, &ours); err != nil {
		t.Fatal(err)
	}
	if err := decideSharePrepare(false, shareIdentity{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := decideProtect(mountpointFacts{Exists: true, NonEmpty: true, Device: "1:1"}, nil); !errors.Is(err, errShareBlocked) {
		t.Fatal("nonempty unknown directory was accepted")
	}
	if err := decideProtect(mountpointFacts{Exists: true, Symlink: true}, &ours); !errors.Is(err, errShareBlocked) {
		t.Fatal("symlink mountpoint was accepted")
	}
	recorded := ours
	recorded.UnderlyingDevice = "3:4"
	if err := decideProtect(mountpointFacts{Exists: true, Device: "9:9", Mode: "0", UID: 0}, &recorded); !errors.Is(err, errShareBlocked) {
		t.Fatal("replaced underlying directory was accepted")
	}
	if err := decideProtect(mountpointFacts{Exists: true, Device: "3:4", Mode: "0", UID: 0}, &recorded); err != nil {
		t.Fatal(err)
	}
	if err := decideProtect(mountpointFacts{Exists: true, UID: 0, Mode: "0", Device: "1:2"}, nil); err != nil {
		t.Fatal("empty root-owned placeholder was rejected", err)
	}
}

func TestSavedHostRootRejectsReplacement(t *testing.T) {
	profile := t.TempDir()
	runtime, err := Configure(profile, fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	first := t.TempDir()
	second := t.TempDir()
	info, dev, ino, err := captureHostRoot(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.storeHostRoot(hostRootRecord{Path: first, Dev: dev, Ino: ino}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runtime.authorizeHostRoot(second); !errors.Is(err, errShareBlocked) {
		t.Fatalf("replaced environment directory was authorized: %v", err)
	}
	reloaded, err := runtime.loadHostRoot()
	if err != nil || reloaded == nil || reloaded.Path != first || reloaded.Dev != dev || reloaded.Ino != ino {
		t.Fatalf("saved host root was rewritten: %+v %v", reloaded, err)
	}
	got, gotDev, gotIno, err := runtime.authorizeHostRoot(first)
	if err != nil || gotDev != dev || gotIno != ino || !os.SameFile(info, got) {
		t.Fatalf("original root was refused: %v dev=%d ino=%d", err, gotDev, gotIno)
	}
	left, right := net.Pipe()
	defer right.Close()
	err = reversesshfs.ServeConfinedPinned(left, second, false, nil, info)
	_ = left.Close()
	if !errors.Is(err, reversesshfs.ErrPinnedRootMismatch) {
		t.Fatalf("replaced root was served: %v", err)
	}
	if err = reversesshfs.ServeConfinedPinned(right, first, false, nil, nil); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil identity was served: %v", err)
	}
}

func TestShareScriptsRefuseForeignMountAndUnprotectedDirectory(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to execute the guest cleanup scripts")
	}
	mount := t.TempDir()
	if err := os.WriteFile(filepath.Join(mount, "data"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "sudo.log")
	bin := t.TempDir()
	writeFake(t, bin, "stat", `#!/bin/sh
printf '%s\n' "${DEVICE:-4:4}"
`)
	writeFake(t, bin, "sudo", `#!/bin/sh
if [ "$1" = -n ]; then shift; fi
printf '%s\n' "$*" >> "$LOG"
if [ "$1" = fusermount3 ]; then : > "$UNMOUNTED"; fi
exit 0
`)
	writeFake(t, bin, "findmnt", `#!/bin/sh
if [ -n "$UNMOUNTED" ] && [ -f "$UNMOUNTED" ]; then exit 1; fi
prev=
for arg in "$@"; do
  if [ "$prev" = -o ] && [ "$arg" = FSTYPE ]; then printf '%s\n' "$FSTYPE"; exit 0; fi
  if [ "$prev" = -o ] && [ "$arg" = SOURCE ]; then printf '%s\n' "$SOURCE"; exit 0; fi
  prev=$arg
done
exit 0
`)
	command := exec.Command("bash", "-c", shareProtectScript(mount, ""))
	command.Env = []string{"PATH=" + bin, "LOG=" + log, "DEVICE=4:4"}
	if err := command.Run(); err == nil {
		t.Fatal("nonempty mount directory was modified")
	}
	if body, err := os.ReadFile(log); err == nil && len(body) > 0 {
		t.Fatalf("protect script mutated a foreign directory: %s", body)
	}

	ours := shareIdentity{Mountpoint: mount, Source: ":" + mount, FSType: "fuse.sshfs", MountDevice: "7:9"}
	script, err := shareUnmountScript(mount, ours)
	if err != nil {
		t.Fatal(err)
	}
	foreignLog := filepath.Join(t.TempDir(), "foreign.log")
	foreign := exec.Command("bash", "-c", script)
	foreign.Env = []string{"PATH=" + bin, "LOG=" + foreignLog, "FSTYPE=ext4", "SOURCE=:" + mount, "DEVICE=7:9", "UNMOUNTED=" + filepath.Join(t.TempDir(), "foreign-unmounted")}
	if err := foreign.Run(); err == nil {
		t.Fatal("ext4 mount was unmounted")
	}
	if body, err := os.ReadFile(foreignLog); err == nil && len(body) > 0 {
		t.Fatalf("foreign mount was mutated: %s", body)
	}
	matchedLog := filepath.Join(t.TempDir(), "matched.log")
	matched := exec.Command("bash", "-c", script)
	matched.Env = []string{"PATH=" + bin, "LOG=" + matchedLog, "FSTYPE=fuse.sshfs", "SOURCE=:" + mount, "DEVICE=7:9", "UNMOUNTED=" + filepath.Join(t.TempDir(), "matched-unmounted")}
	if err := matched.Run(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(matchedLog)
	if err != nil || !strings.Contains(string(body), "fusermount3 -u") {
		t.Fatalf("matching sshfs mount was not unmounted: %q %v", body, err)
	}
}

func writeFake(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
}
