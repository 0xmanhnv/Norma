//go:build linux

package tool

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// holdOrphan backgrounds a memory holder from a subshell that exits at once:
// the holder is reparented to pid 1 and so leaves the task's ppid tree, but
// stays in its process group — the set treeKill actually kills. The trailing
// ":" in each branch stops the shell from exec-ing the final sleep in place,
// which would drop the allocation the test depends on.
const holdOrphan = `( { x=a; while [ ${#x} -lt 200000000 ]; do x="$x$x"; done; sleep 30; :; } & ) ; sleep 30; :`

// forkShared allocates once and then forks five subshells that hold the same
// pages copy-on-write. Summing per-process RSS counts those pages six times;
// the real footprint is one copy.
const forkShared = `x=a; while [ ${#x} -lt 50000000 ]; do x="$x$x"; done; for i in 1 2 3 4 5; do { sleep 120; :; } & done; sleep 120; :`

// A leaker whose parent exits must still be caught. Measuring the ppid tree
// instead of the process group loses it the moment init adopts it: the walk
// reports the few MiB of the surviving shell while the orphan holds hundreds,
// and the guard never fires on the shape a shell script produces every time it
// starts its real work with "cmd &".
func TestMemGuardCatchesOrphanedDescendant(t *testing.T) {
	t.Setenv("AGENT_CORE_TASK_MEM_CAP", "64m")
	m := newTestManager(t)
	task, err := m.Spawn(SpawnSpec{Command: holdOrphan})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	m.Background(task.ID)
	info := waitTerminalIn(t, m, task.ID, 30*time.Second)
	if info.Status != TaskFailed || info.Reason != terminateMemory {
		t.Fatalf("status=%s reason=%q, want failed/memory — the orphan left the ppid tree but not the process group", info.Status, info.Reason)
	}
}

// The inverse: a parent that forks workers shares its pages, and summing RSS
// over the group multiplies them by the number of holders. Killing on that sum
// would take down ordinary multiprocessing work and tell the model to rewrite a
// command that never leaked, so a group over the cheap bound must be confirmed
// against Pss before it dies.
//
// This is the guard's decision, tested on fixed numbers: a group reading
// 880 MiB by RSS and 145 MiB by Pss under a 400 MiB cap is spared, pass after
// pass; the same group is killed once Pss is over the cap too, and when Pss
// cannot be read at all. The measures themselves are checked against a real
// forking workload in TestMemGuardSparesSharedPages.
func TestMemGuardVerdictNeedsBothMeasures(t *testing.T) {
	t.Setenv("AGENT_CORE_TASK_MEM_CAP", "400m")
	cases := []struct {
		name     string
		rss, pss int64
		pssOK    bool
		wantKill bool
	}{
		{"shared pages: over by RSS, under by Pss", 880 << 20, 145 << 20, true, false},
		{"exactly at the cap by Pss", 880 << 20, 400 << 20, true, false},
		{"a real overrun: over by both", 880 << 20, 500 << 20, true, true},
		{"Pss unreadable: RSS decides", 880 << 20, 0, false, true},
		{"under the cap by RSS: Pss is not consulted", 300 << 20, 900 << 20, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTestManager(t)
			pssCalls := 0
			m.mu.Lock()
			m.memRSS = func(pgids []int) map[int]int64 {
				out := map[int]int64{}
				for _, g := range pgids {
					out[g] = c.rss
				}
				return out
			}
			m.memPSS = func(int) (int64, bool) { pssCalls++; return c.pss, c.pssOK }
			m.mu.Unlock()
			task, err := m.Spawn(SpawnSpec{Command: "sleep 60"})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			m.Background(task.ID)
			for range 5 { // several passes with the group sitting where it is
				m.memGuardPass()
			}
			info, _ := m.Get(task.ID)
			killed := info.Status == TaskFailed && info.Reason == terminateMemory
			if killed != c.wantKill {
				t.Fatalf("status=%s reason=%q after 5 passes (rss=%dMiB pss=%dMiB ok=%v), want killed=%v",
					info.Status, info.Reason, c.rss>>20, c.pss>>20, c.pssOK, c.wantKill)
			}
			if !c.wantKill && info.Status.Terminal() {
				t.Fatalf("the task ended as %s/%s", info.Status, info.Reason)
			}
			if c.rss <= 400<<20 && pssCalls != 0 {
				t.Fatalf("Pss was sampled %d times for a group under the cap by RSS", pssCalls)
			}
			m.Kill(task.ID)
		})
	}
}

// The measures behind that decision, on a real workload: a shell that
// allocates once and forks five holders of the same pages reads several times
// its footprint by RSS and about once by Pss, and a guard whose cap lies
// between the two leaves it alone.
//
// Nothing here depends on how much memory the workload ends up holding on
// this machine, or on how long it takes to get there. The guard is off while
// the workload grows (a shell doubling a string briefly holds several copies
// of it, which is a real footprint and not what this test is about). Once the
// two measures have settled apart, the cap is put halfway between what was
// measured, and the guard's passes are run directly. If the measures never
// separate — smaps_rollup unreadable, the pages swapped out or never shared —
// the precondition cannot be established and the test says so and skips.
func TestMemGuardSparesSharedPages(t *testing.T) {
	t.Setenv("AGENT_CORE_TASK_MEM_CAP", "off")
	m := newTestManager(t)
	task, err := m.Spawn(SpawnSpec{Command: forkShared})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	m.Background(task.ID)
	defer m.Kill(task.ID)

	// Wait for the measures to diverge and hold still: while the parent is
	// still growing its string alone there is nothing shared yet and RSS ==
	// Pss, and while it forks the group changes between two reads. Two
	// consecutive samples that agree mean the workload is at rest.
	pgid := task.cmd.Process.Pid
	separated := func(rss, pss int64, ok bool) bool {
		return ok && pss >= 16<<20 && rss >= 3*pss
	}
	var rss, pss, prevPSS int64
	var ok, settled bool
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && !settled {
		if info, found := m.Get(task.ID); found && info.Status.Terminal() {
			t.Fatalf("the workload ended as %s/%s with the guard off (rss=%dMiB pss=%dMiB)",
				info.Status, info.Reason, rss>>20, pss>>20)
		}
		time.Sleep(200 * time.Millisecond)
		rss = pgrpRSS([]int{pgid})[pgid]
		pss, ok = pgrpPSS(pgid)
		if d := pss - prevPSS; separated(rss, pss, ok) && d < 8<<20 && d > -(8<<20) {
			settled = true
		}
		prevPSS = pss
	}
	if !settled {
		t.Skipf("precondition not established: the group's RSS never settled at three times its Pss on this host "+
			"(rss=%dMiB pss=%dMiB readable=%v); shared pages cannot be told from private ones here", rss>>20, pss>>20, ok)
	}

	// A cap halfway between the two: over by the cheap measure, under by the
	// accurate one, with as much margin as this workload gives on each side.
	limit := (rss + pss) / 2
	t.Setenv("AGENT_CORE_TASK_MEM_CAP", strconv.FormatInt(limit, 10))
	if got := memCapBytes(); got != limit {
		t.Fatalf("cap = %d, want %d", got, limit)
	}
	for range 3 {
		m.memGuardPass()
		if info, _ := m.Get(task.ID); info.Status.Terminal() {
			t.Fatalf("killed a group whose real footprint (%d MiB Pss) is under the %s cap; only its double-counted RSS (%d MiB) was over",
				pss>>20, humanBytes(limit), rss>>20)
		}
	}

	// And the guard is not simply idle: with the cap under the real footprint
	// the same group is killed.
	t.Setenv("AGENT_CORE_TASK_MEM_CAP", strconv.FormatInt(pss/2, 10))
	m.memGuardPass()
	if info := waitTerminalIn(t, m, task.ID, 10*time.Second); info.Status != TaskFailed || info.Reason != terminateMemory {
		t.Fatalf("status=%s reason=%q with the cap at half the Pss, want failed/memory", info.Status, info.Reason)
	}
}

// pgrpRSS must see every member of the group, including one the task's own
// parent-child links no longer reach.
func TestPgrpRSSCoversWholeGroup(t *testing.T) {
	m := newTestManager(t)
	task, err := m.Spawn(SpawnSpec{Command: holdOrphan})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer m.Kill(task.ID)
	pgid := task.cmd.Process.Pid

	var measured, want int64
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		measured = pgrpRSS([]int{pgid})[pgid]
		want = groupRSSViaProc(t, pgid)
		if want > 100<<20 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if want <= 100<<20 {
		t.Skipf("holder never became resident on this host (%d MiB)", want>>20)
	}
	// Both read /proc a moment apart, so allow drift; the failure this guards
	// against is an order-of-magnitude miss, not a few MiB.
	if diff := want - measured; diff > 32<<20 || diff < -(32<<20) {
		t.Fatalf("pgrpRSS = %d MiB, independent group sum = %d MiB", measured>>20, want>>20)
	}
}

// groupRSSViaProc sums VmRSS over the process group, reading /proc/<pid>/status
// rather than the stat field pgrpRSS parses, so the two do not share a bug.
func groupRSSViaProc(t *testing.T, pgid int) int64 {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	var total int64
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		g, err := syscall.Getpgid(pid)
		if err != nil || g != pgid {
			continue
		}
		b, err := os.ReadFile(procFile(pid, "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "VmRSS:") {
				continue
			}
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					total += kb << 10
				}
			}
			break
		}
	}
	return total
}

// The derived default must be a usable cap, not a constant and not something
// that would kill ordinary work on the machine running the tests.
func TestDefaultMemCapFollowsHostBudget(t *testing.T) {
	budget, ok := hostMemBudget()
	if !ok {
		t.Skip("host memory budget unreadable")
	}
	if budget < minMemCap {
		t.Skipf("host budget %s is below the floor", humanBytes(budget))
	}
	got := defaultMemCap()
	if got > budget {
		t.Fatalf("cap %s exceeds the budget %s it is meant to protect", humanBytes(got), humanBytes(budget))
	}
	if got < minMemCap {
		t.Fatalf("cap %s is below the floor %s", humanBytes(got), humanBytes(minMemCap))
	}
}
