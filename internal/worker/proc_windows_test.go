package worker

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// A timed-out job must take the agent's children down with it, not only the
// process it started.
func TestTimeoutKillsProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("AGENTLINK_FAKE_PIDFILE", pidFile)
	rec := newRecorder()
	w := newWorker(t, fakeAgent(t, "tree").Runner(), rec, t.TempDir(), 2*time.Second)
	start(t, w)
	accept(t, w, msg(id1, "hang with a child"))
	if got := rec.wait(t, 1)[0]; !strings.Contains(got.Body, "timed out") {
		t.Fatalf("reply = %+v", got)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return // already gone
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if ev, _ := windows.WaitForSingleObject(h, 5000); ev != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(h, 1)
		t.Fatalf("child process %d survived the timeout", pid)
	}
}
