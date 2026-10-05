package runner

import (
	"os"
	"os/exec"
	"testing"
)

// A process started in a runner job (attach, via startProc) is an agent
// caller while the job lives; this test process and a closed job's process
// are not.
func TestInAgentJob(t *testing.T) {
	if InAgentJob(uint32(os.Getpid())) { //nolint:gosec // G115: own pid
		t.Fatal("own process reported inside a runner job")
	}
	cmd := exec.CommandContext(t.Context(), "ping.exe", "-n", "60", "127.0.0.1")
	p, err := startProc(cmd, "", func([]byte) {}, func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	pid := uint32(cmd.Process.Pid) //nolint:gosec // G115: a started pid
	t.Cleanup(func() { _ = p.finish() })
	if !InAgentJob(pid) {
		t.Fatalf("child %d in a live runner job not detected", pid)
	}
	if InAgentJob(uint32(os.Getpid())) { //nolint:gosec // G115: own pid
		t.Fatal("own process reported inside a runner job while one is live")
	}
	if err := p.finish(); err != nil {
		t.Fatal(err)
	}
	if InAgentJob(pid) {
		t.Fatalf("pid %d still reported after its job closed", pid)
	}
	liveTrees.Lock()
	n := len(liveTrees.m)
	liveTrees.Unlock()
	if n != 0 {
		t.Fatalf("%d trees left registered", n)
	}
}
