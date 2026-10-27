package fsutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultTxnStateDir is the default dir that holds the batch transaction journals.
const DefaultTxnStateDir = ".fsutil-txn"

const (
	txnStatePrepared   = "prepared"
	txnStateCommitting = "committing"

	txnJournalPrefix = "txn-"
	txnJournalSuffix = ".json"
)

// TxnTransformFunc receives the original file contents and returns the new contents.
//
//   - return the same bytes (or nil) to mark the file as unchanged: it will not be
//     staged, rewritten or touched in any way, its mtime and permissions stay as-is.
//   - note is a human readable description of the change, eg: "set keys: a, b",
//     it is shown in the TxnPlan and recorded in the journal for crash recovery.
//   - returning an error aborts the whole plan before anything is written.
type TxnTransformFunc func(filePath string, oldBytes []byte) (newBytes []byte, note string, err error)

// TxnChange describes the planned result for one file.
type TxnChange struct {
	// Path is the absolute target file path.
	Path string
	// Changed reports whether the new contents differ from the old ones.
	Changed bool
	// Note is the change description returned by the TxnTransformFunc.
	Note string
	// OldSize is the original file size in bytes.
	OldSize int64
	// NewSize is the staged file size in bytes, equals OldSize when not Changed.
	NewSize int64
}

// TxnPlan is the inspection result of a BatchTxn. Nothing has been applied yet.
type TxnPlan struct {
	// ID is the transaction id, used in journal and temp sidecar file names.
	ID string
	// StateDir holds the journal of this transaction.
	StateDir string
	// Changes lists every registered file, including unchanged ones.
	Changes []TxnChange
}

// ChangedList returns only the files that will actually be rewritten on Commit.
func (p *TxnPlan) ChangedList() []TxnChange {
	out := make([]TxnChange, 0, len(p.Changes))
	for _, c := range p.Changes {
		if c.Changed {
			out = append(out, c)
		}
	}
	return out
}

// TxnRecoverReport reports what RecoverTxns did for one unfinished transaction.
type TxnRecoverReport struct {
	// ID is the recovered transaction id.
	ID string
	// Outcome is "new" (all files resolved to the new contents) or
	// "old" (all files resolved to the original contents).
	Outcome string
	// Resolved is the number of files that were reconciled.
	Resolved int
	// Committed counts files finally holding the new contents.
	Committed int
	// RolledBack counts files finally holding the original contents.
	RolledBack int
}

// BatchTxnOption for NewBatchTxn.
type BatchTxnOption struct {
	// StateDir is the dir that stores the transaction journals.
	// Default see DefaultTxnStateDir.
	StateDir string
}

// BatchTxnOptFunc for configure BatchTxn.
type BatchTxnOptFunc func(*BatchTxnOption)

// WithTxnStateDir custom the dir that stores the transaction journals.
func WithTxnStateDir(dir string) BatchTxnOptFunc {
	return func(opt *BatchTxnOption) {
		opt.StateDir = dir
	}
}

// NewBatchTxn create a new BatchTxn instance.
//
// Workflow:
//
//  1. Add all target files with their transform func.
//  2. Plan() inspects every file and stages all new contents. Any read/transform
//     error aborts before touching originals, no temp file is left.
//  3. Review the plan, then Commit() to apply all changes at once,
//     or Rollback() to discard everything.
//  4. On the next process startup, call RecoverTxns() first to finish any
//     interrupted transaction, leaving the directory either all-old or all-new.
func NewBatchTxn(optFns ...BatchTxnOptFunc) *BatchTxn {
	opt := &BatchTxnOption{StateDir: DefaultTxnStateDir}
	for _, fn := range optFns {
		fn(opt)
	}
	return &BatchTxn{
		stateDir:   opt.StateDir,
		transforms: make(map[string]TxnTransformFunc),
	}
}

// txnFilePlan is one file entry persisted in the journal.
type txnFilePlan struct {
	Path    string `json:"path"`
	Tmp     string `json:"tmp"`
	Bak     string `json:"bak"`
	Perm    uint32 `json:"perm"`
	Changed bool   `json:"changed"`
	Done    bool   `json:"done"`
	Note    string `json:"note"`
	OldSize int64  `json:"old_size"`
	NewSize int64  `json:"new_size"`
}

// txnJournal is persisted as JSON, every state transition is written atomically.
type txnJournal struct {
	ID    string         `json:"id"`
	State string         `json:"state"`
	Files []*txnFilePlan `json:"files"`
}

// BatchTxn updates a batch of files atomically.
//
// The commit of each file uses a rename dance with sidecar files in the same
// directory as the target (same filesystem, so renames are atomic):
//
//	original -> backup, staged -> original
//
// All state is recorded in a journal, so a crash at any point can be resolved
// by RecoverTxns to either the complete old or the complete new contents.
type BatchTxn struct {
	stateDir string

	id          string
	journalPath string
	journal     *txnJournal

	transforms map[string]TxnTransformFunc
	order      []string

	planned  bool
	finished bool
}

// Add registers one file and its transform func.
//
// The file must exist and be a regular file (symlinks are rejected). Add must
// be called before Plan; adding the same path twice returns an error.
func (t *BatchTxn) Add(filePath string, fn TxnTransformFunc) error {
	if fn == nil {
		return errors.New("fsutil: BatchTxn.Add: the transform func is nil")
	}
	if t.planned {
		return errors.New("fsutil: BatchTxn.Add: plan already created, cannot add more files")
	}
	if t.finished {
		return errors.New("fsutil: BatchTxn.Add: transaction already finished")
	}

	abs, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Errorf("fsutil: BatchTxn.Add: resolve path %q: %w", filePath, err)
	}
	if _, ok := t.transforms[abs]; ok {
		return fmt.Errorf("fsutil: BatchTxn.Add: file %q added twice", abs)
	}

	t.transforms[abs] = fn
	t.order = append(t.order, abs)
	return nil
}

// MustAdd like Add, panic on error.
func (t *BatchTxn) MustAdd(filePath string, fn TxnTransformFunc) {
	if err := t.Add(filePath, fn); err != nil {
		panic(err)
	}
}

// Plan inspects every registered file and stages all new contents.
//
// All files are read and all transform funcs run in memory first; only when
// every one succeeds are the staged temp files and the journal written.
// Originals are never modified here. Call Commit or Rollback afterwards.
func (t *BatchTxn) Plan() (*TxnPlan, error) {
	if t.planned {
		return nil, errors.New("fsutil: BatchTxn.Plan: plan already created")
	}
	if t.finished {
		return nil, errors.New("fsutil: BatchTxn.Plan: transaction already finished")
	}
	if len(t.order) == 0 {
		return nil, errors.New("fsutil: BatchTxn.Plan: no files added")
	}

	t.id = fmt.Sprintf("%x", time.Now().UnixNano())

	// phase 1: inspect everything in memory, so a bad file aborts cleanly.
	type staged struct {
		entry *txnFilePlan
		data  []byte
	}
	stagedFiles := make([]*staged, 0, len(t.order))

	for _, path := range t.order {
		li, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("fsutil: BatchTxn.Plan: stat %q: %w", path, err)
		}
		if li.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("fsutil: BatchTxn.Plan: %q is a symlink, only regular files are allowed", path)
		}
		if !li.Mode().IsRegular() {
			return nil, fmt.Errorf("fsutil: BatchTxn.Plan: %q is not a regular file", path)
		}

		old, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("fsutil: BatchTxn.Plan: read %q: %w", path, err)
		}

		newBs, note, err := t.transforms[path](path, old)
		if err != nil {
			return nil, fmt.Errorf("fsutil: BatchTxn.Plan: transform %q: %w", path, err)
		}

		entry := &txnFilePlan{
			Path:    path,
			Perm:    uint32(li.Mode().Perm()),
			Note:    note,
			OldSize: int64(len(old)),
			NewSize: int64(len(newBs)),
		}
		if string(newBs) == string(old) {
			entry.NewSize = entry.OldSize
		} else {
			entry.Changed = true
			dir, base := filepath.Split(path)
			entry.Tmp = filepath.Join(dir, fmt.Sprintf(".%s.fstxnew.%s", base, t.id))
			entry.Bak = filepath.Join(dir, fmt.Sprintf(".%s.fstxbak.%s", base, t.id))
		}
		stagedFiles = append(stagedFiles, &staged{entry: entry, data: newBs})
	}

	// phase 2: all transforms succeeded, stage the new contents.
	if err := os.MkdirAll(t.stateDir, DefaultDirPerm); err != nil {
		return nil, fmt.Errorf("fsutil: BatchTxn.Plan: create state dir %q: %w", t.stateDir, err)
	}

	j := &txnJournal{ID: t.id, State: txnStatePrepared}
	for _, s := range stagedFiles {
		j.Files = append(j.Files, s.entry)
		if s.entry.Changed {
			if err := writeStagedFile(s.entry.Tmp, s.data, os.FileMode(s.entry.Perm)); err != nil {
				// nothing has been moved yet, originals are all intact.
				_ = t.removeStaged(j.Files)
				return nil, fmt.Errorf("fsutil: BatchTxn.Plan: stage %q: %w", s.entry.Path, err)
			}
		}
	}

	t.journal = j
	t.journalPath = filepath.Join(t.stateDir, txnJournalPrefix+t.id+txnJournalSuffix)
	if err := t.saveJournal(); err != nil {
		_ = t.removeStaged(j.Files)
		return nil, err
	}

	t.planned = true

	plan := &TxnPlan{ID: t.id, StateDir: t.stateDir}
	for _, e := range j.Files {
		plan.Changes = append(plan.Changes, TxnChange{
			Path:    e.Path,
			Changed: e.Changed,
			Note:    e.Note,
			OldSize: e.OldSize,
			NewSize: e.NewSize,
		})
	}
	return plan, nil
}

// Commit applies all staged changes.
//
// Unchanged files are skipped entirely. For every changed file the original is
// renamed to a backup and the staged contents are renamed into its place;
// renames are atomic so every file is either fully old or fully new.
//
// If any rename fails, the already swapped files are restored from their
// backups synchronously and all sidecars and the journal are removed, so the
// directory ends in the complete old state. A crash (power loss, kill -9) is
// resolved forward to the complete new state by RecoverTxns on next startup.
func (t *BatchTxn) Commit() error {
	if t.finished {
		return errors.New("fsutil: BatchTxn.Commit: transaction already finished")
	}
	if !t.planned {
		return errors.New("fsutil: BatchTxn.Commit: call Plan first")
	}

	t.journal.State = txnStateCommitting
	if err := t.saveJournal(); err != nil {
		return err
	}

	var commitErr error
	for _, e := range t.journal.Files {
		if !e.Changed {
			continue
		}

		if err := os.Rename(e.Path, e.Bak); err != nil {
			commitErr = fmt.Errorf("fsutil: BatchTxn.Commit: backup %q: %w", e.Path, err)
			break
		}
		if err := os.Rename(e.Tmp, e.Path); err != nil {
			// put the original back before rolling the earlier files back.
			_ = os.Rename(e.Bak, e.Path)
			commitErr = fmt.Errorf("fsutil: BatchTxn.Commit: install new contents for %q: %w", e.Path, err)
			break
		}

		e.Done = true
		// persist progress: recovery knows exactly which files are already new.
		_ = t.saveJournal()
	}

	if commitErr != nil {
		rbErr := t.rollbackAfterFailure()
		t.finished = true
		if rbErr != nil {
			return fmt.Errorf("%w; rollback failed: %v", commitErr, rbErr)
		}
		return commitErr
	}

	if err := t.removeBackups(); err != nil {
		return err
	}
	if err := t.removeJournal(); err != nil {
		return err
	}
	t.finished = true
	return nil
}

// Rollback discards a prepared plan. All originals stay untouched, staged
// files and the journal are removed.
func (t *BatchTxn) Rollback() error {
	if t.finished {
		return errors.New("fsutil: BatchTxn.Rollback: transaction already finished")
	}
	if !t.planned {
		return errors.New("fsutil: BatchTxn.Rollback: call Plan first")
	}

	if err := t.removeStaged(t.journal.Files); err != nil {
		return err
	}
	if err := t.removeJournal(); err != nil {
		return err
	}
	t.finished = true
	return nil
}

// rollbackAfterFailure restores files already swapped during a failed Commit.
func (t *BatchTxn) rollbackAfterFailure() error {
	var errs []string

	// restore in reverse order.
	for i := len(t.journal.Files) - 1; i >= 0; i-- {
		e := t.journal.Files[i]
		if !e.Changed || !e.Done {
			continue
		}

		// new contents are at Path, original at Bak. Move new back to Tmp
		// first, then restore the original, so no window with a missing file.
		if fileExists(e.Path) {
			if err := os.Rename(e.Path, e.Tmp); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Sprintf("set aside new %q: %v", e.Path, err))
			}
		}
		if fileExists(e.Bak) {
			if err := os.Rename(e.Bak, e.Path); err != nil {
				errs = append(errs, fmt.Sprintf("restore %q: %v", e.Path, err))
			}
		}
		e.Done = false
		_ = t.saveJournal()
	}

	if err := t.removeStaged(t.journal.Files); err != nil {
		errs = append(errs, err.Error())
	}
	if err := t.removeJournal(); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// saveJournal writes the journal atomically (temp file + rename).
func (t *BatchTxn) saveJournal() error {
	data, err := json.MarshalIndent(t.journal, "", "  ")
	if err != nil {
		return fmt.Errorf("fsutil: BatchTxn: encode journal: %w", err)
	}

	tmp := t.journalPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("fsutil: BatchTxn: write journal %q: %w", tmp, err)
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if err1 := f.Close(); err == nil {
		err = err1
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fsutil: BatchTxn: write journal %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, t.journalPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("fsutil: BatchTxn: finalize journal %q: %w", t.journalPath, err)
	}
	return nil
}

// removeStaged removes staged temp files (never originals or backups).
func (t *BatchTxn) removeStaged(files []*txnFilePlan) error {
	var errs []string
	for _, e := range files {
		if !e.Changed || e.Tmp == "" {
			continue
		}
		if err := os.Remove(e.Tmp); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Sprintf("remove %q: %v", e.Tmp, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// removeBackups removes the backup sidecars after every swap succeeded.
func (t *BatchTxn) removeBackups() error {
	var errs []string
	for _, e := range t.journal.Files {
		if !e.Changed {
			continue
		}
		if err := os.Remove(e.Bak); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Sprintf("remove %q: %v", e.Bak, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// removeJournal deletes the journal file and best-effort removes the state dir
// when it is empty.
func (t *BatchTxn) removeJournal() error {
	if err := os.Remove(t.journalPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(t.stateDir)
	return nil
}

// writeStagedFile writes new contents to a temp sidecar next to the target,
// syncs it and applies the original file's permission bits.
func writeStagedFile(tmpPath string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}

	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if err1 := f.Close(); err == nil {
		err = err1
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	// create perm is affected by umask, set the original mode explicitly.
	if err := os.Chmod(tmpPath, perm); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// RecoverTxns reconciles unfinished transactions left by a crashed process.
//
// It must be called before starting a new batch on the next startup.
//
//   - journal state "prepared": the commit never started, all staged files are
//     removed and every target keeps its original contents (complete old state).
//   - journal state "committing": files are resolved forward so every target
//     ends with the new contents (complete new state). Per-file Done flags in
//     the journal and the presence of backup/staged sidecars make the exact
//     crash point unambiguous.
//
// All temp files, backups and journals are removed afterwards; no temp file is
// ever left behind. Returns one report per resolved journal.
func RecoverTxns(stateDir ...string) ([]TxnRecoverReport, error) {
	dir := DefaultTxnStateDir
	if len(stateDir) > 0 && stateDir[0] != "" {
		dir = stateDir[0]
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fsutil: RecoverTxns: read state dir %q: %w", dir, err)
	}

	var reports []TxnRecoverReport
	var errs []string

	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() {
			continue
		}

		// temp of an unfinished journal write: no transaction was ever live.
		if strings.HasSuffix(name, txnJournalSuffix+".tmp") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Sprintf("remove %q: %v", name, err))
			}
			continue
		}
		if !strings.HasPrefix(name, txnJournalPrefix) || !strings.HasSuffix(name, txnJournalSuffix) {
			continue
		}

		report, err := recoverOne(filepath.Join(dir, name))
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		reports = append(reports, report)
	}

	if len(errs) > 0 {
		return reports, errors.New(strings.Join(errs, "; "))
	}
	_ = os.Remove(dir)
	return reports, nil
}

func recoverOne(journalPath string) (TxnRecoverReport, error) {
	report := TxnRecoverReport{}

	data, err := os.ReadFile(journalPath)
	if err != nil {
		return report, fmt.Errorf("fsutil: RecoverTxns: read journal %q: %w", journalPath, err)
	}

	var j txnJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return report, fmt.Errorf("fsutil: RecoverTxns: parse journal %q: %w", journalPath, err)
	}
	report.ID = j.ID

	// prepared -> complete old state; committing -> complete new state.
	toNew := j.State == txnStateCommitting
	if toNew {
		report.Outcome = "new"
	} else {
		report.Outcome = "old"
	}

	var errs []string
	for _, e := range j.Files {
		if !e.Changed {
			report.Resolved++
			report.RolledBack++
			continue
		}

		pathOK := fileExists(e.Path)
		bakOK := fileExists(e.Bak)
		tmpOK := fileExists(e.Tmp)
		endsNew := false

		switch {
		case toNew && e.Done:
			// the swap finished before the crash: new contents are at Path.
			switch {
			case pathOK:
				endsNew = true
			case tmpOK:
				// crashed between the two renames, finish the second one.
				if err := os.Rename(e.Tmp, e.Path); err != nil {
					errs = append(errs, fmt.Sprintf("finish %q: %v", e.Path, err))
					continue
				}
				tmpOK, pathOK = false, true
				endsNew = true
			case bakOK:
				// staged contents are lost, cannot go forward: keep the original.
				if err := os.Rename(e.Bak, e.Path); err != nil {
					errs = append(errs, fmt.Sprintf("restore %q: %v", e.Path, err))
					continue
				}
				bakOK, pathOK = false, true
			}
		case toNew && !e.Done:
			// swap never started for this entry, complete it forward.
			switch {
			case pathOK && tmpOK && !bakOK:
				if err := os.Rename(e.Path, e.Bak); err != nil {
					errs = append(errs, fmt.Sprintf("backup %q: %v", e.Path, err))
					continue
				}
				if err := os.Rename(e.Tmp, e.Path); err != nil {
					// best effort undo of the first rename.
					_ = os.Rename(e.Bak, e.Path)
					errs = append(errs, fmt.Sprintf("install %q: %v", e.Path, err))
					continue
				}
				pathOK, bakOK, tmpOK = true, true, false
				endsNew = true
			case !pathOK && bakOK && tmpOK:
				if err := os.Rename(e.Tmp, e.Path); err != nil {
					errs = append(errs, fmt.Sprintf("finish %q: %v", e.Path, err))
					continue
				}
				pathOK, tmpOK = true, false
				endsNew = true
			case pathOK && !tmpOK:
				// staged file lost: cannot go new, keep the original.
				if bakOK {
					if err := os.Remove(e.Bak); err != nil && !os.IsNotExist(err) {
						errs = append(errs, fmt.Sprintf("remove %q: %v", e.Bak, err))
					}
					bakOK = false
				}
			case pathOK && tmpOK && bakOK:
				// ambiguous state that the normal commit/recovery flow cannot
				// produce: do not destroy any sidecar, keep the journal for
				// manual inspection.
				errs = append(errs, fmt.Sprintf("ambiguous state for %q (path, tmp and backup all exist)", e.Path))
				continue
			}
		default:
			// resolve to old: restore from backup when Path is missing.
			if !pathOK && bakOK {
				if err := os.Rename(e.Bak, e.Path); err != nil {
					errs = append(errs, fmt.Sprintf("restore %q: %v", e.Path, err))
					continue
				}
				bakOK, pathOK = false, true
			}
		}

		// cleanup every leftover sidecar for this entry.
		if bakOK {
			if err := os.Remove(e.Bak); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Sprintf("remove %q: %v", e.Bak, err))
			}
		}
		if tmpOK {
			if err := os.Remove(e.Tmp); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Sprintf("remove %q: %v", e.Tmp, err))
			}
		}
		if !pathOK {
			errs = append(errs, fmt.Sprintf("%q missing after recovery", e.Path))
			continue
		}

		report.Resolved++
		if endsNew {
			report.Committed++
		} else {
			report.RolledBack++
		}
	}

	// journal is only removed after all its sidecars were reconciled; on any
	// error it is kept on disk so the state can be inspected or retried.
	if len(errs) > 0 {
		return report, errors.New(strings.Join(errs, "; "))
	}
	if err := os.Remove(journalPath); err != nil && !os.IsNotExist(err) {
		return report, fmt.Errorf("remove journal %q: %w", journalPath, err)
	}
	return report, nil
}
