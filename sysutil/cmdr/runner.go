package cmdr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gookit/goutil/arrutil"
	"github.com/gookit/goutil/cliutil/cmdline"
	"github.com/gookit/goutil/errorx"
	"github.com/gookit/goutil/maputil"
	"github.com/gookit/goutil/mathutil"
	"github.com/gookit/goutil/strutil/textutil"
	"github.com/gookit/goutil/x/ccolor"
)

// per-task run errors, can check by errors.Is
var (
	// ErrTaskTimeout task run exceeded its configured timeout
	ErrTaskTimeout = errors.New("cmdr: task run timeout")
	// ErrTaskCanceled task was canceled, before start or while running
	ErrTaskCanceled = errors.New("cmdr: task run canceled")
)

// Task struct
type Task struct {
	err   error
	index int

	// ID for task
	ID  string
	Cmd *Cmd

	// Timeout max run duration of the task. zero means no per-task
	// timeout, fallback to Runner.TaskTimeout on concurrent run.
	Timeout time.Duration

	// BeforeRun hook
	BeforeRun func(t *Task)
	PrevCond  func(prev *Task) bool
}

// NewTask instance
func NewTask(cmd *Cmd) *Task {
	return &Task{
		Cmd: cmd,
	}
}

// get task id by cmd.Name
func (t *Task) ensureID(idx int) {
	t.index = idx
	if t.ID != "" {
		return
	}

	id := t.Cmd.IDString()
	if t.Cmd.Name == "" {
		id += mathutil.String(idx)
	}
	t.ID = id
}

var rpl = textutil.NewVarReplacer("$").DisableFlatten()

// RunWith command
func (t *Task) RunWith(ctx maputil.Data) error {
	cmdVars := ctx.StringMap("cmdVars")

	if len(cmdVars) > 0 {
		// rpl := strutil.NewReplacer(cmdVars)
		for i, val := range t.Cmd.Args {
			if strings.ContainsRune(val, '$') {
				t.Cmd.Args[i] = rpl.RenderSimple(val, cmdVars)
			}
		}
	}

	return t.Run()
}

// Run command
func (t *Task) Run() error {
	if t.BeforeRun != nil {
		t.BeforeRun(t)
	}

	t.err = t.Cmd.Run()
	return t.err
}

// RunCtx run command with context. supports render cmd vars like RunWith.
func (t *Task) RunCtx(ctx context.Context, data maputil.Data) error {
	if len(data) > 0 {
		cmdVars := data.StringMap("cmdVars")

		if len(cmdVars) > 0 {
			for i, val := range t.Cmd.Args {
				if strings.ContainsRune(val, '$') {
					t.Cmd.Args[i] = rpl.RenderSimple(val, cmdVars)
				}
			}
		}
	}

	if t.BeforeRun != nil {
		t.BeforeRun(t)
	}

	t.err = t.Cmd.RunCtx(ctx)
	return t.err
}

// Err get
func (t *Task) Err() error {
	return t.err
}

// Index get
func (t *Task) Index() int {
	return t.index
}

// Cmdline get
func (t *Task) Cmdline() string {
	return t.Cmd.Cmdline()
}

// IsSuccess of task
func (t *Task) IsSuccess() bool {
	return t.err == nil
}

// TaskResult store the run result of a task on concurrent run
type TaskResult struct {
	// ID task name
	ID string
	// Index in the runner task list
	Index int
	// Cmdline the executed command line
	Cmdline string
	// Output combined stdout and stderr of the task, captured separately
	Output string
	// Err nil on success. timeout/canceled can check by IsTimeout/IsCanceled
	Err error
	// Elapsed task run duration
	Elapsed time.Duration
	// Skipped the task was skipped by BeforeRun hook
	Skipped bool
}

// IsSuccess of the task result
func (r *TaskResult) IsSuccess() bool { return r.Err == nil && !r.Skipped }

// IsTimeout of the task result
func (r *TaskResult) IsTimeout() bool { return errors.Is(r.Err, ErrTaskTimeout) }

// IsCanceled of the task result
func (r *TaskResult) IsCanceled() bool { return errors.Is(r.Err, ErrTaskCanceled) }

// IsSkipped of the task result
func (r *TaskResult) IsSkipped() bool { return r.Skipped }

// RunnerHookFn func
type RunnerHookFn func(r *Runner, t *Task) bool

// Runner use for batch run multi task commands
type Runner struct {
	prev *Task
	// task name to index
	idMap map[string]int
	tasks []*Task
	// Errs on run tasks, key is Task.ID
	Errs errorx.ErrMap
	// Results of last ConcurrentRun, order is same as added tasks
	Results []*TaskResult

	// Workers max concurrent tasks for ConcurrentRun. default 3.
	Workers int
	// TaskTimeout default per-task timeout on concurrent run,
	// for tasks without own Timeout. zero means no timeout.
	TaskTimeout time.Duration

	// Workdir common workdir
	Workdir string
	// EnvMap will append to task.Cmd on run
	EnvMap map[string]string

	// Params for add custom params
	Params maputil.Map

	// DryRun dry run all commands
	DryRun bool
	// OutToStd stdout and stderr
	OutToStd bool
	// IgnoreErr continue on error
	IgnoreErr bool
	// BeforeRun hooks on each task. return false to skip current task.
	//
	// On ConcurrentRun, hooks are invoked in worker goroutines, so they
	// must be safe for concurrent use.
	BeforeRun func(r *Runner, t *Task) bool
	// AfterRun hook on each task. return false to stop running.
	//
	// On ConcurrentRun, return false cancels the batch: no new tasks
	// start and in-flight tasks are killed.
	AfterRun func(r *Runner, t *Task) bool
}

// NewRunner instance with config func
func NewRunner(fns ...func(rr *Runner)) *Runner {
	rr := &Runner{
		idMap:  make(map[string]int, 0),
		tasks:  make([]*Task, 0),
		Errs:   make(errorx.ErrMap),
		Params: make(maputil.Map),
	}

	rr.OutToStd = true
	for _, fn := range fns {
		fn(rr)
	}
	return rr
}

// WithOutToStd set
func (r *Runner) WithOutToStd() *Runner {
	r.OutToStd = true
	return r
}

// WithWorkers set max concurrent workers for ConcurrentRun
func (r *Runner) WithWorkers(n int) *Runner {
	r.Workers = n
	return r
}

// WithTaskTimeout set default per-task timeout for ConcurrentRun
func (r *Runner) WithTaskTimeout(d time.Duration) *Runner {
	r.TaskTimeout = d
	return r
}

// Add multitask at once
func (r *Runner) Add(tasks ...*Task) *Runner {
	for _, task := range tasks {
		r.AddTask(task)
	}
	return r
}

// AddTask add one task
func (r *Runner) AddTask(task *Task) *Runner {
	if task.Cmd == nil {
		panic("task command cannot be empty")
	}

	idx := len(r.tasks)
	task.ensureID(idx)

	// TODO check id repeat
	r.idMap[task.ID] = idx
	r.tasks = append(r.tasks, task)
	return r
}

// AddCmd commands
func (r *Runner) AddCmd(cmds ...*Cmd) *Runner {
	for _, cmd := range cmds {
		r.AddTask(&Task{Cmd: cmd})
	}
	return r
}

// GitCmd quick a git command task
func (r *Runner) GitCmd(subCmd string, args ...string) *Runner {
	return r.AddTask(&Task{
		Cmd: NewGitCmd(subCmd, args...),
	})
}

// CmdWithArgs a command task
func (r *Runner) CmdWithArgs(cmdName string, args ...string) *Runner {
	return r.AddTask(&Task{
		Cmd: NewCmd(cmdName, args...),
	})
}

// CmdWithAnys a command task
func (r *Runner) CmdWithAnys(cmdName string, args ...any) *Runner {
	return r.AddTask(&Task{
		Cmd: NewCmd(cmdName, arrutil.SliceToStrings(args)...),
	})
}

// AddCmdline as a command task
func (r *Runner) AddCmdline(line string) *Runner {
	bin, args := cmdline.NewParser(line).BinAndArgs()

	return r.AddTask(&Task{
		Cmd: NewCmd(bin, args...),
	})
}

// Run all tasks
func (r *Runner) Run() error {
	// do run tasks
	for i, task := range r.tasks {
		if r.BeforeRun != nil && !r.BeforeRun(r, task) {
			continue
		}

		if r.prev != nil && task.PrevCond != nil && !task.PrevCond(r.prev) {
			continue
		}

		if r.DryRun {
			ccolor.Infof("DRY-RUN: task#%d execute completed\n\n", i+1)
			continue
		}

		if !r.RunTask(task) {
			break
		}
		fmt.Println() // with newline.
	}

	if len(r.Errs) == 0 {
		return nil
	}
	return r.Errs
}

// StepRun one command
func (r *Runner) StepRun() error {
	return nil // TODO
}

// RunTask command
func (r *Runner) RunTask(task *Task) (goon bool) {
	if len(r.EnvMap) > 0 {
		task.Cmd.AppendEnv(r.EnvMap)
	}

	if r.OutToStd && !task.Cmd.HasStdout() {
		task.Cmd.ToOSStdoutStderr()
	}

	// common workdir
	if r.Workdir != "" && task.Cmd.Dir == "" {
		task.Cmd.WithWorkDir(r.Workdir)
	}

	// do running
	if err := task.RunWith(r.Params); err != nil {
		r.Errs[task.ID] = err
		ccolor.Errorf("Task#%d run error: %s\n", task.Index()+1, err)

		// not ignore error, stop.
		if !r.IgnoreErr {
			return false
		}
	}

	if r.AfterRun != nil && !r.AfterRun(r, task) {
		return false
	}

	// store prev
	r.prev = task
	return true
}

// ConcurrentRun run all tasks concurrently, at most r.Workers (default 3)
// tasks run at the same time.
//
// Each task has its own timeout (Task.Timeout, fallback Runner.TaskTimeout);
// a failed or timeout task does NOT stop other tasks. When ctx is canceled,
// no more new tasks start, and in-flight tasks are killed (results are
// marked as ErrTaskCanceled).
//
// Each task's stdout+stderr is captured separately into TaskResult.Output,
// outputs never mix. When r.OutToStd is true, a per-task report with the
// full output is printed after all tasks finished.
//
// Tasks are independent: Task.PrevCond is ignored on concurrent run.
// The returned results keep the add order; the returned error is the
// r.Errs map (keyed by task ID) when any task failed.
func (r *Runner) ConcurrentRun(ctx context.Context) (results []*TaskResult, err error) {
	results = make([]*TaskResult, len(r.tasks))
	r.Results = results
	r.Errs = make(errorx.ErrMap)

	if len(r.tasks) == 0 {
		return results, nil
	}

	workers := r.Workers
	if workers <= 0 {
		workers = 3
	}
	if workers > len(r.tasks) {
		workers = len(r.tasks)
	}

	batchCtx, cancelBatch := context.WithCancel(ctx)
	defer cancelBatch()

	jobs := make(chan int)
	var wg sync.WaitGroup
	var errMu sync.Mutex

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				r.runConcurrentOne(batchCtx, results, idx, &errMu, cancelBatch)
			}
		}()
	}

feed:
	for i := range r.tasks {
		select {
		case <-batchCtx.Done():
			// caller canceled (or a AfterRun hook stopped the batch):
			// the not-started tasks will not run.
			for j := i; j < len(r.tasks); j++ {
				results[j] = r.markCanceled(r.tasks[j], &errMu)
			}
			break feed
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()

	if r.OutToStd {
		printTaskResults(results)
	}

	return results, r.Errs.ErrorOrNil()
}

// runConcurrentOne execute a single task in worker, capture output separately.
func (r *Runner) runConcurrentOne(
	ctx context.Context,
	results []*TaskResult,
	idx int,
	errMu *sync.Mutex,
	cancelBatch context.CancelFunc,
) {
	task := r.tasks[idx]
	start := time.Now()

	defer func() {
		if rec := recover(); rec != nil {
			resErr := fmt.Errorf("cmdr: task %q panic recover: %v", task.ID, rec)
			task.err = resErr
			results[idx] = &TaskResult{
				ID:      task.ID,
				Index:   task.index,
				Cmdline: task.Cmdline(),
				Err:     resErr,
				Elapsed: time.Since(start),
			}

			errMu.Lock()
			r.Errs[task.ID] = resErr
			errMu.Unlock()
		}
	}()

	if r.BeforeRun != nil && !r.BeforeRun(r, task) {
		results[idx] = &TaskResult{
			ID:      task.ID,
			Index:   task.index,
			Cmdline: task.Cmdline(),
			Skipped: true,
		}
		return
	}

	res := &TaskResult{
		ID:      task.ID,
		Index:   task.index,
		Cmdline: task.Cmdline(),
	}

	// common env and workdir, same as sequential RunTask
	if len(r.EnvMap) > 0 {
		task.Cmd.AppendEnv(r.EnvMap)
	}
	if r.Workdir != "" && task.Cmd.Dir == "" {
		task.Cmd.WithWorkDir(r.Workdir)
	}

	if r.DryRun {
		res.Elapsed = time.Since(start)
		res.Output = "DRY-RUN: ok"
		results[idx] = res
		return
	}

	// per-task timeout
	var taskCtx context.Context
	var cancel context.CancelFunc
	if timeout := r.taskTimeout(task); timeout > 0 {
		taskCtx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		taskCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	// capture stdout+stderr into own buffer; restore user writers afterwards
	var buf bytes.Buffer
	oldOut, oldErrOut := task.Cmd.Stdout, task.Cmd.Stderr
	task.Cmd.WithOutput(&buf, &buf)
	defer func() {
		task.Cmd.Stdout = oldOut
		task.Cmd.Stderr = oldErrOut
	}()

	runErr := task.RunCtx(taskCtx, r.Params)
	res.Elapsed = time.Since(start)
	res.Output = buf.String()

	if runErr != nil {
		res.Err = wrapTaskErr(runErr, task.ID)
		task.err = res.Err

		errMu.Lock()
		r.Errs[task.ID] = res.Err
		errMu.Unlock()
	}
	results[idx] = res

	if r.AfterRun != nil && !r.AfterRun(r, task) {
		cancelBatch()
	}
}

// markCanceled build result for a task that never started
func (r *Runner) markCanceled(task *Task, errMu *sync.Mutex) *TaskResult {
	resErr := fmt.Errorf("%w: task %q", ErrTaskCanceled, task.ID)
	task.err = resErr

	errMu.Lock()
	r.Errs[task.ID] = resErr
	errMu.Unlock()

	return &TaskResult{
		ID:      task.ID,
		Index:   task.index,
		Cmdline: task.Cmdline(),
		Err:     resErr,
	}
}

// wrapTaskErr map context error to sentinel errors
func wrapTaskErr(err error, id string) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: task %q", ErrTaskTimeout, id)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: task %q", ErrTaskCanceled, id)
	default:
		return err
	}
}

// taskTimeout resolve effective per-task timeout
func (r *Runner) taskTimeout(task *Task) time.Duration {
	if task.Timeout > 0 {
		return task.Timeout
	}
	return r.TaskTimeout
}

// printTaskResults print per-task report with full output, in add order
func printTaskResults(results []*TaskResult) {
	for _, res := range results {
		fmt.Println()

		if res.Skipped {
			ccolor.Infof("==== Task [%s] #%d SKIPPED\n", res.ID, res.Index+1)
			continue
		}

		if res.Err == nil {
			ccolor.Successf("==== Task [%s] #%d SUCCESS (%s)\n", res.ID, res.Index+1, res.Elapsed)
		} else {
			ccolor.Errorf("==== Task [%s] #%d FAILED (%s) %s\n", res.ID, res.Index+1, res.Elapsed, res.Err)
		}

		ccolor.Infoln("CMD:", res.Cmdline)
		if output := strings.TrimRight(res.Output, "\n"); output != "" {
			fmt.Println(output)
		}
	}
}

// Len of tasks
func (r *Runner) Len() int {
	return len(r.tasks)
}

// Reset instance
func (r *Runner) Reset() *Runner {
	r.prev = nil
	r.tasks = make([]*Task, 0)
	r.idMap = make(map[string]int, 0)
	return r
}

// TaskIDs get
func (r *Runner) TaskIDs() []string {
	ss := make([]string, 0, len(r.idMap))
	for id := range r.idMap {
		ss = append(ss, id)
	}
	return ss
}

// Prev task instance after running
func (r *Runner) Prev() *Task {
	return r.prev
}

// Task get by id name
func (r *Runner) Task(id string) (*Task, error) {
	if idx, ok := r.idMap[id]; ok {
		return r.tasks[idx], nil
	}
	return nil, fmt.Errorf("task %q is not exists", id)
}
