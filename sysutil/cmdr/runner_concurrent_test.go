package cmdr_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gookit/goutil/sysutil/cmdr"
	"github.com/gookit/goutil/x/assert"
)

// helperProcessEnv marks a re-spawned test binary as helper process
const helperProcessEnv = "GO_CMDR_TEST_HELPER"

// helperCmd builds a portable command by re-spawning the test binary itself,
// so tests do not depend on platform shell builtins (echo/sleep/...).
//
// actions:
//
//	echo <text...>           print text to stdout, exit 0
//	echoerr <text...>        print text to stderr, exit 0
//	sleep <ms> [text] [code] sleep ms, optional print text, optional exit code
//	stamp <dir> <ms>         record start time file in dir, then sleep ms
func helperCmd(args ...string) *cmdr.Cmd {
	c := exec.Command(os.Args[0], append([]string{"-test.run=TestHelperProcess", "--"}, args...)...)
	c.Env = append(os.Environ(), helperProcessEnv+"=1")
	return cmdr.WrapGoCmd(c)
}

// TestHelperProcess is the re-spawned helper process entry, not a real test.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperProcessEnv) != "1" {
		return
	}

	var rest []string
	for i, a := range os.Args {
		if a == "--" {
			rest = os.Args[i+1:]
			break
		}
	}

	if len(rest) == 0 {
		os.Exit(2)
	}

	switch rest[0] {
	case "echo":
		fmt.Println(strings.Join(rest[1:], " "))
		os.Exit(0)
	case "echoerr":
		fmt.Fprintln(os.Stderr, strings.Join(rest[1:], " "))
		os.Exit(0)
	case "sleep":
		if len(rest) < 2 {
			os.Exit(2)
		}
		ms, _ := strconv.Atoi(rest[1])
		time.Sleep(time.Duration(ms) * time.Millisecond)

		if len(rest) > 2 {
			fmt.Println(rest[2])
		}
		if len(rest) > 3 {
			if code, err := strconv.Atoi(rest[3]); err == nil {
				os.Exit(code)
			}
		}
		os.Exit(0)
	case "stamp":
		// stamp <dir> <ms>
		if len(rest) < 3 {
			os.Exit(2)
		}
		name := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
		if err := os.WriteFile(filepath.Join(rest[1], name), []byte("s"), 0o600); err != nil {
			os.Exit(2)
		}

		ms, _ := strconv.Atoi(rest[2])
		time.Sleep(time.Duration(ms) * time.Millisecond)
		os.Exit(0)
	}

	os.Exit(2)
}

func TestRunner_ConcurrentRun_success(t *testing.T) {
	rr := cmdr.NewRunner(func(r *cmdr.Runner) { r.OutToStd = false })
	rr.AddTask(&cmdr.Task{ID: "alpha", Cmd: helperCmd("echo", "AAA-OUTPUT")})
	rr.AddTask(&cmdr.Task{ID: "beta", Cmd: helperCmd("echoerr", "BBB-OUTPUT")})
	rr.AddTask(&cmdr.Task{ID: "gamma", Cmd: helperCmd("echo", "CCC-OUTPUT")})

	results, err := rr.ConcurrentRun(context.Background())
	assert.NoErr(t, err)
	assert.Len(t, results, 3)

	// results keep add order
	assert.Eq(t, "alpha", results[0].ID)
	assert.Eq(t, "beta", results[1].ID)
	assert.Eq(t, "gamma", results[2].ID)

	for _, res := range results {
		assert.True(t, res.IsSuccess(), "task %s should success: %v", res.ID, res.Err)
		assert.False(t, res.IsTimeout())
		assert.False(t, res.IsCanceled())
	}

	// outputs captured separately, never mixed
	assert.StrContains(t, results[0].Output, "AAA-OUTPUT")
	assert.NotContains(t, results[0].Output, "BBB-OUTPUT")
	assert.NotContains(t, results[0].Output, "CCC-OUTPUT")

	assert.StrContains(t, results[1].Output, "BBB-OUTPUT")
	assert.NotContains(t, results[1].Output, "AAA-OUTPUT")

	assert.StrContains(t, results[2].Output, "CCC-OUTPUT")
	assert.NotContains(t, results[2].Output, "BBB-OUTPUT")

	// stored on runner as well
	assert.Eq(t, len(rr.Results), len(results))
}

func TestRunner_ConcurrentRun_failureIsolated(t *testing.T) {
	rr := cmdr.NewRunner(func(r *cmdr.Runner) { r.OutToStd = false })
	rr.AddTask(&cmdr.Task{ID: "ok1", Cmd: helperCmd("echo", "GOOD-1")})
	rr.AddTask(&cmdr.Task{ID: "bad", Cmd: helperCmd("sleep", "30", "", "3")})
	rr.AddTask(&cmdr.Task{ID: "ok2", Cmd: helperCmd("echo", "GOOD-2")})

	results, err := rr.ConcurrentRun(context.Background())

	// a failed task does not stop other tasks
	assert.Err(t, err)
	assert.Len(t, results, 3)
	assert.True(t, results[0].IsSuccess())
	assert.True(t, results[2].IsSuccess())

	bad := results[1]
	assert.Eq(t, "bad", bad.ID)
	assert.Err(t, bad.Err)
	assert.False(t, bad.IsSuccess())
	assert.StrContains(t, bad.Err.Error(), "3") // exit code 3

	// errs keyed by task ID
	task, e := rr.Task("bad")
	assert.NoErr(t, e)
	assert.Err(t, task.Err())
	assert.False(t, task.IsSuccess())
}

func TestRunner_ConcurrentRun_taskTimeout(t *testing.T) {
	rr := cmdr.NewRunner(func(r *cmdr.Runner) {
		r.OutToStd = false
		r.WithTaskTimeout(time.Hour) // runner default should be overridden by task
	})
	rr.AddTask(&cmdr.Task{ID: "fast", Cmd: helperCmd("echo", "FAST")})
	rr.AddTask(&cmdr.Task{
		ID:      "slow",
		Cmd:     helperCmd("sleep", "3000"),
		Timeout: 120 * time.Millisecond,
	})
	rr.AddTask(&cmdr.Task{ID: "fast2", Cmd: helperCmd("echo", "FAST2")})

	start := time.Now()
	results, err := rr.ConcurrentRun(context.Background())
	elapsed := time.Since(start)

	assert.Err(t, err)
	assert.Len(t, results, 3)
	assert.True(t, results[0].IsSuccess())
	assert.True(t, results[2].IsSuccess())

	slow := results[1]
	assert.True(t, slow.IsTimeout(), "slow task should timeout, err=%v", slow.Err)
	assert.True(t, errors.Is(slow.Err, cmdr.ErrTaskTimeout))
	assert.False(t, slow.IsCanceled())

	// timeout is per-task: batch should return much earlier than 3s
	assert.True(t, elapsed < 2*time.Second, "elapsed=%v", elapsed)
}

func TestRunner_ConcurrentRun_runnerDefaultTimeout(t *testing.T) {
	rr := cmdr.NewRunner(func(r *cmdr.Runner) {
		r.OutToStd = false
		r.WithTaskTimeout(120 * time.Millisecond)
	})
	rr.AddTask(&cmdr.Task{ID: "slow", Cmd: helperCmd("sleep", "3000")})

	results, err := rr.ConcurrentRun(context.Background())
	assert.Err(t, err)
	assert.Len(t, results, 1)
	assert.True(t, results[0].IsTimeout())
}

func TestRunner_ConcurrentRun_workersLimit(t *testing.T) {
	dir := t.TempDir()

	const taskN = 6
	const sleepMS = 300

	rr := cmdr.NewRunner(func(r *cmdr.Runner) {
		r.OutToStd = false
		r.WithWorkers(3)
	})
	for i := 0; i < taskN; i++ {
		name := "stamp-" + strconv.Itoa(i)
		rr.AddTask(&cmdr.Task{ID: name, Cmd: helperCmd("stamp", dir, strconv.Itoa(sleepMS))})
	}

	start := time.Now()
	results, err := rr.ConcurrentRun(context.Background())
	elapsed := time.Since(start)

	assert.NoErr(t, err)
	assert.Len(t, results, taskN)
	for _, res := range results {
		assert.True(t, res.IsSuccess(), "task %s err=%v", res.ID, res.Err)
	}

	// at least two waves with 3 workers
	assert.True(t, elapsed >= 2*sleepMS*time.Millisecond-100*time.Millisecond,
		"elapsed=%v should be at least two waves", elapsed)
	assert.True(t, elapsed < 10*time.Second, "elapsed=%v", elapsed)

	// collect task start times recorded by helper processes
	entries, err := os.ReadDir(dir)
	assert.NoErr(t, err)
	assert.Len(t, entries, taskN)

	starts := make([]int64, 0, taskN)
	for _, ent := range entries {
		nano, err := strconv.ParseInt(strings.SplitN(ent.Name(), "-", 2)[0], 10, 64)
		assert.NoErr(t, err)
		starts = append(starts, nano)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	// first three tasks start close together
	firstWaveGap := time.Duration(starts[2]-starts[0]) * time.Nanosecond
	assert.True(t, firstWaveGap < 500*time.Millisecond, "first wave gap=%v", firstWaveGap)

	// the 4th task only starts after one of first wave finishes
	waveGap := time.Duration(starts[3]-starts[2]) * time.Nanosecond
	assert.True(t, waveGap >= 180*time.Millisecond,
		"4th task started too soon, concurrency limit not respected: %v", waveGap)
}

func TestRunner_ConcurrentRun_cancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	rr := cmdr.NewRunner(func(r *cmdr.Runner) {
		r.OutToStd = false
		r.WithWorkers(3)
	})
	for i := 0; i < 6; i++ {
		rr.AddTask(&cmdr.Task{
			ID:  "long-" + strconv.Itoa(i),
			Cmd: helperCmd("sleep", "5000"),
		})
	}

	start := time.Now()
	results, err := rr.ConcurrentRun(ctx)
	elapsed := time.Since(start)

	assert.Err(t, err)
	assert.Len(t, results, 6)

	// in-flight tasks must be stopped: batch returns well before 5s
	assert.True(t, elapsed < 3*time.Second, "elapsed=%v", elapsed)

	for _, res := range results {
		assert.NotEmpty(t, res)
		assert.True(t, res.IsCanceled(),
			"task %s should be canceled, err=%v timeout=%v", res.ID, res.Err, res.IsTimeout())
		assert.True(t, errors.Is(res.Err, cmdr.ErrTaskCanceled))
	}
}

func TestRunner_ConcurrentRun_beforeRunSkip(t *testing.T) {
	rr := cmdr.NewRunner(func(r *cmdr.Runner) {
		r.OutToStd = false
		r.BeforeRun = func(_ *cmdr.Runner, task *cmdr.Task) bool {
			return task.ID != "skip-me"
		}
	})
	rr.AddTask(&cmdr.Task{ID: "run-me", Cmd: helperCmd("echo", "RAN")})
	rr.AddTask(&cmdr.Task{ID: "skip-me", Cmd: helperCmd("echo", "SHOULD-NOT-RUN")})

	results, err := rr.ConcurrentRun(context.Background())
	assert.NoErr(t, err)
	assert.Len(t, results, 2)
	assert.True(t, results[0].IsSuccess())
	assert.True(t, results[1].IsSkipped())
	assert.False(t, results[1].IsSuccess())
}

func TestRunner_ConcurrentRun_dryRun(t *testing.T) {
	rr := cmdr.NewRunner(func(r *cmdr.Runner) {
		r.OutToStd = false
		r.DryRun = true
	})
	rr.AddTask(&cmdr.Task{ID: "t1", Cmd: helperCmd("echo", "X")})

	results, err := rr.ConcurrentRun(context.Background())
	assert.NoErr(t, err)
	assert.True(t, results[0].IsSuccess())
	assert.Eq(t, "DRY-RUN: ok", results[0].Output)
}

func TestRunner_ConcurrentRun_reportToStd(t *testing.T) {
	// smoke test: per-task report should print without panic
	rr := cmdr.NewRunner() // OutToStd defaults true
	rr.AddTask(&cmdr.Task{ID: "ok", Cmd: helperCmd("echo", "REPORT-OK")})
	rr.AddTask(&cmdr.Task{ID: "fail", Cmd: helperCmd("sleep", "10", "", "2")})

	results, err := rr.ConcurrentRun(context.Background())
	assert.Err(t, err)
	assert.Len(t, results, 2)
	assert.True(t, results[0].IsSuccess())
	assert.Err(t, results[1].Err)
}
