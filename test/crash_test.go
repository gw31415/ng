//go:build integration

package test

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Shougo/ng/internal/nvim"
	"github.com/Shougo/ng/internal/screen"
)

// TestHelperProcess plays the part of ng: it starts Neovim exactly as ng
// does, types text without saving, reports the server address and then
// waits to be killed. It only runs when started by TestSurvivesGUIKill.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("NG_TEST_HELPER") != "1" {
		t.Skip("helper process")
	}
	nv, err := nvim.Start(nvimBin(), "", []string{"--clean", "-n", "-i", "NONE"}, nil, nil)
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	go nv.Serve()
	if err := nv.Attach(40, 6); err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	if _, err := nv.Call("nvim_input", "iunsaved work<Esc>"); err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	for i := 0; i < 250; i++ {
		v, _ := nv.Call("nvim_buf_get_lines", 0, 0, 1, false)
		if l, ok := v.([]any); ok && len(l) == 1 && l[0] == "unsaved work" {
			fmt.Println("ADDR", nv.Addr)
			select {} // wait for SIGKILL
		}
		time.Sleep(20 * time.Millisecond)
	}
	fmt.Println("ERROR text never appeared")
	os.Exit(1)
}

func nvimBin() string {
	if b := os.Getenv("NG_TEST_NVIM"); b != "" {
		return b
	}
	return "nvim"
}

// ng is killed with SIGKILL, so none of its cleanup runs: exactly what a
// real crash looks like. Neovim must survive with the unsaved buffer, and
// "ng --server" (FindServers/Orphans) must find it.
func TestSurvivesGUIKill(t *testing.T) {
	if _, err := exec.LookPath(nvimBin()); err != nil {
		t.Skip("nvim not found")
	}
	// The helper's sockets go to a private directory, so other Neovim
	// instances on the machine do not disturb the "orphan" check.
	// Not t.TempDir(): on macOS its path is too long for a Unix socket
	// (104 bytes).
	dir, err := os.MkdirTemp("", "ng")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "NG_TEST_HELPER=1", "XDG_RUNTIME_DIR="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var addr string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "ERROR") {
			t.Fatal(line)
		}
		if a, ok := strings.CutPrefix(line, "ADDR "); ok {
			addr = a
			break
		}
	}
	if addr == "" {
		t.Fatal("helper did not report an address")
	}

	// Crash the "GUI".
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	// The orphaned server is found the way "ng --server" finds it.
	var orphans []nvim.Server
	for i := 0; i < 100; i++ {
		orphans = nvim.Orphans(nvim.FindServers())
		if len(orphans) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(orphans) != 1 || orphans[0].Addr != addr {
		t.Fatalf("orphans %+v, want exactly %s", orphans, addr)
	}
	if orphans[0].Modified != 1 {
		t.Errorf("modified buffers %d, want 1", orphans[0].Modified)
	}

	// Attach a new UI and see the unsaved text.
	h := &harness{t: t, scr: screen.New(40, 6), flushed: make(chan struct{}, 1)}
	nv, err := nvim.Connect(addr, h.onNotify, nil)
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	h.nv = nv
	go nv.Serve()
	defer func() {
		nv.Notify("nvim_command", "qall!")
		<-nv.Done()
	}()
	if err := nv.Attach(40, 6); err != nil {
		t.Fatal(err)
	}
	h.waitFor("unsaved text after crash", func(s *screen.Screen) bool { return row(s, 0) == "unsaved work" })

	// Now that a UI is attached it is no longer an orphan.
	if o := nvim.Orphans(nvim.FindServers()); len(o) != 0 {
		t.Errorf("server still reported as orphan: %+v", o)
	}
}
