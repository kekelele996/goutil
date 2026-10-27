package fsutil

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func txNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func txErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func txEq(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func txTrue(t *testing.T, ok bool, msg ...any) {
	t.Helper()
	if !ok {
		t.Fatal("expected true", msg)
	}
}

// writeTxnFile creates a file with the given contents and perm.
func writeTxnFile(t *testing.T, path, contents string, perm os.FileMode) {
	t.Helper()
	txNoErr(t, os.WriteFile(path, []byte(contents), perm))
	// WriteFile applies umask, force the intended mode explicitly.
	txNoErr(t, os.Chmod(path, perm))
}

func readTxnFile(t *testing.T, path string) string {
	t.Helper()
	bs, err := os.ReadFile(path)
	txNoErr(t, err)
	return string(bs)
}

// assertNoTxnLeftovers fails if any temp sidecar or journal remains under dir.
func assertNoTxnLeftovers(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.Contains(name, ".fstxnew.") ||
			strings.Contains(name, ".fstxbak.") ||
			strings.HasPrefix(name, txnJournalPrefix) {
			t.Errorf("txn leftover found: %s", path)
		}
		return nil
	})
	txNoErr(t, err)
}

func TestBatchTxnPlanAndCommit(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")

	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	cDir := filepath.Join(dir, "sub")
	txNoErr(t, os.MkdirAll(cDir, 0755))
	c := filepath.Join(cDir, "c.txt")

	writeTxnFile(t, a, "old-a", 0644)
	writeTxnFile(t, b, "keep-b", 0644)
	writeTxnFile(t, c, "old-c", 0600)

	tx := NewBatchTxn(WithTxnStateDir(stateDir))
	appendMark := func(mark string) TxnTransformFunc {
		return func(_ string, old []byte) ([]byte, string, error) {
			if strings.HasPrefix(string(old), "keep") {
				return old, "unchanged", nil
			}
			return append(old, []byte(mark)...), "append " + mark, nil
		}
	}
	txNoErr(t, tx.Add(a, appendMark("-new")))
	txNoErr(t, tx.Add(b, appendMark("-new")))
	txNoErr(t, tx.Add(c, appendMark("-new")))

	// duplicates and nil funcs are rejected
	txErr(t, tx.Add(a, appendMark("-new")))
	txErr(t, tx.Add(filepath.Join(dir, "x.txt"), nil))

	plan, err := tx.Plan()
	txNoErr(t, err)
	txEq(t, len(plan.Changes), 3)
	txEq(t, len(plan.ChangedList()), 2)

	// after Plan, originals are untouched.
	txEq(t, readTxnFile(t, a), "old-a")
	txEq(t, readTxnFile(t, b), "keep-b")
	txEq(t, readTxnFile(t, c), "old-c")

	// b has no staged sidecar at all: unchanged files are not even staged.
	bSidecars, err := filepath.Glob(filepath.Join(dir, ".b.txt.fstx*"))
	txNoErr(t, err)
	txEq(t, len(bSidecars), 0)

	txNoErr(t, tx.Commit())

	txEq(t, readTxnFile(t, a), "old-a-new")
	txEq(t, readTxnFile(t, b), "keep-b")
	txEq(t, readTxnFile(t, c), "old-c-new")

	// permissions are preserved exactly.
	mustPerm := func(path string) os.FileMode {
		st, err := os.Stat(path)
		txNoErr(t, err)
		return st.Mode().Perm()
	}
	txEq(t, mustPerm(a), os.FileMode(0644))
	txEq(t, mustPerm(b), os.FileMode(0644))
	txEq(t, mustPerm(c), os.FileMode(0600))

	// no temp files, backups or journals remain.
	assertNoTxnLeftovers(t, dir)
	txTrue(t, !PathExists(stateDir))
}

func TestBatchTxnUnchangedFileNotRewritten(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	writeTxnFile(t, a, "old", 0644)
	writeTxnFile(t, b, "same", 0644)

	// pin an old mtime so a rewrite would be observable.
	fixed := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	txNoErr(t, os.Chtimes(b, fixed, fixed))

	tx := NewBatchTxn(WithTxnStateDir(filepath.Join(dir, "state")))
	txNoErr(t, tx.Add(a, func(_ string, old []byte) ([]byte, string, error) {
		return []byte("new"), "rewrite", nil
	}))
	txNoErr(t, tx.Add(b, func(_ string, old []byte) ([]byte, string, error) {
		return old, "", nil
	}))
	_, err := tx.Plan()
	txNoErr(t, err)
	txNoErr(t, tx.Commit())

	st, err := os.Stat(b)
	txNoErr(t, err)
	txTrue(t, st.ModTime().Equal(fixed), "unchanged file mtime must be kept")
	txEq(t, readTxnFile(t, b), "same")
}

func TestBatchTxnBadFileAbortsClean(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	a := filepath.Join(dir, "a.txt")
	missing := filepath.Join(dir, "missing.txt")
	writeTxnFile(t, a, "old", 0644)

	tx := NewBatchTxn(WithTxnStateDir(stateDir))
	change := func(_ string, old []byte) ([]byte, string, error) {
		return []byte("new"), "rewrite", nil
	}
	txNoErr(t, tx.Add(a, change))
	txNoErr(t, tx.Add(missing, change))

	_, err := tx.Plan()
	txErr(t, err)

	// original intact, no sidecars, no state dir created.
	txEq(t, readTxnFile(t, a), "old")
	assertNoTxnLeftovers(t, dir)
	txTrue(t, !PathExists(stateDir))
}

func TestBatchTxnTransformErrorAbortsClean(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	writeTxnFile(t, a, "old-a", 0644)
	writeTxnFile(t, b, "old-b", 0644)

	tx := NewBatchTxn(WithTxnStateDir(stateDir))
	txNoErr(t, tx.Add(a, func(_ string, old []byte) ([]byte, string, error) {
		return []byte("new-a"), "a", nil
	}))
	txNoErr(t, tx.Add(b, func(_ string, _ []byte) ([]byte, string, error) {
		return nil, "", errors.New("bad file contents")
	}))

	_, err := tx.Plan()
	txErr(t, err)
	txEq(t, readTxnFile(t, a), "old-a")
	txEq(t, readTxnFile(t, b), "old-b")
	assertNoTxnLeftovers(t, dir)
	txTrue(t, !PathExists(stateDir))
}

func TestBatchTxnSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	writeTxnFile(t, real, "old", 0644)
	txNoErr(t, os.Symlink(real, link))

	tx := NewBatchTxn(WithTxnStateDir(filepath.Join(dir, "state")))
	txNoErr(t, tx.Add(link, func(_ string, old []byte) ([]byte, string, error) {
		return []byte("new"), "rewrite", nil
	}))
	_, err := tx.Plan()
	txErr(t, err)
	txEq(t, readTxnFile(t, real), "old")
}

func TestBatchTxnRollback(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	a := filepath.Join(dir, "a.txt")
	writeTxnFile(t, a, "old", 0600)

	tx := NewBatchTxn(WithTxnStateDir(stateDir))
	txNoErr(t, tx.Add(a, func(_ string, old []byte) ([]byte, string, error) {
		return []byte("new"), "rewrite", nil
	}))
	_, err := tx.Plan()
	txNoErr(t, err)

	txNoErr(t, tx.Rollback())
	txEq(t, readTxnFile(t, a), "old")
	st, err := os.Stat(a)
	txNoErr(t, err)
	txEq(t, st.Mode().Perm(), os.FileMode(0600))
	assertNoTxnLeftovers(t, dir)
	txTrue(t, !PathExists(stateDir))

	// a finished transaction cannot be driven further.
	txErr(t, tx.Rollback())
	txErr(t, tx.Commit())
}

func TestBatchTxnRerunDoesNotStack(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	writeTxnFile(t, a, "v1", 0644)

	mkTxn := func() *BatchTxn {
		return NewBatchTxn(WithTxnStateDir(filepath.Join(dir, "state")))
	}

	// first run changes v1 -> v2.
	tx := mkTxn()
	txNoErr(t, tx.Add(a, func(_ string, _ []byte) ([]byte, string, error) {
		return []byte("v2"), "set v2", nil
	}))
	plan, err := tx.Plan()
	txNoErr(t, err)
	txEq(t, len(plan.ChangedList()), 1)
	txNoErr(t, tx.Commit())
	txEq(t, readTxnFile(t, a), "v2")

	// second run with the same target value finds nothing to change.
	tx2 := mkTxn()
	txNoErr(t, tx2.Add(a, func(_ string, old []byte) ([]byte, string, error) {
		if string(old) == "v2" {
			return old, "", nil
		}
		return []byte("v2"), "set v2", nil
	}))
	plan, err = tx2.Plan()
	txNoErr(t, err)
	txEq(t, len(plan.ChangedList()), 0)
	txNoErr(t, tx2.Rollback())
	txEq(t, readTxnFile(t, a), "v2")
	assertNoTxnLeftovers(t, dir)
}

func TestRecoverTxnsNoStateDir(t *testing.T) {
	reports, err := RecoverTxns(filepath.Join(t.TempDir(), "nope"))
	txNoErr(t, err)
	txEq(t, len(reports), 0)
}

func TestRecoverPreparedTxnToOld(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	txNoErr(t, os.MkdirAll(stateDir, 0755))

	id := "1111aaaa"
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	writeTxnFile(t, a, "old-a", 0644)
	writeTxnFile(t, b, "old-b", 0600)

	j := &txnJournal{ID: id, State: txnStatePrepared}
	for _, item := range []struct {
		path, old string
		perm      os.FileMode
	}{
		{a, "old-a", 0644}, {b, "old-b", 0600},
	} {
		entry := crashSidecarEntry(id, item.path, item.perm)
		writeTxnFile(t, entry.Tmp, item.old+"-new", item.perm)
		j.Files = append(j.Files, entry)
	}
	writeCrashJournal(t, stateDir, j)

	reports, err := RecoverTxns(stateDir)
	txNoErr(t, err)
	txEq(t, len(reports), 1)
	txEq(t, reports[0].Outcome, "old")
	txEq(t, reports[0].RolledBack, 2)

	txEq(t, readTxnFile(t, a), "old-a")
	txEq(t, readTxnFile(t, b), "old-b")
	st, err := os.Stat(b)
	txNoErr(t, err)
	txEq(t, st.Mode().Perm(), os.FileMode(0600))
	assertNoTxnLeftovers(t, dir)
	txTrue(t, !PathExists(stateDir))
}

func TestRecoverCommittingTxnToNew(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	txNoErr(t, os.MkdirAll(stateDir, 0755))

	id := "2222bbbb"
	a := filepath.Join(dir, "a.txt") // fully swapped before crash: new at path, old at backup
	b := filepath.Join(dir, "b.txt") // swap never started: old at path, new staged
	c := filepath.Join(dir, "c.txt") // crashed between the two renames: no path, old at backup, new staged

	aEntry := crashSidecarEntry(id, a, 0644)
	bEntry := crashSidecarEntry(id, b, 0644)
	cEntry := crashSidecarEntry(id, c, 0600)
	aEntry.Done = true
	cEntry.Done = true

	writeTxnFile(t, a, "new-a", 0644)
	writeTxnFile(t, aEntry.Bak, "old-a", 0644)

	writeTxnFile(t, b, "old-b", 0644)
	writeTxnFile(t, bEntry.Tmp, "new-b", 0644)

	writeTxnFile(t, cEntry.Bak, "old-c", 0600)
	writeTxnFile(t, cEntry.Tmp, "new-c", 0600)

	j := &txnJournal{
		ID:    id,
		State: txnStateCommitting,
		Files: []*txnFilePlan{aEntry, bEntry, cEntry},
	}
	writeCrashJournal(t, stateDir, j)

	reports, err := RecoverTxns(stateDir)
	txNoErr(t, err)
	txEq(t, len(reports), 1)
	txEq(t, reports[0].Outcome, "new")
	txEq(t, reports[0].Committed, 3)
	txEq(t, reports[0].RolledBack, 0)

	txEq(t, readTxnFile(t, a), "new-a")
	txEq(t, readTxnFile(t, b), "new-b")
	txEq(t, readTxnFile(t, c), "new-c")

	st, err := os.Stat(c)
	txNoErr(t, err)
	txEq(t, st.Mode().Perm(), os.FileMode(0600))

	assertNoTxnLeftovers(t, dir)
	txTrue(t, !PathExists(stateDir))
}

func TestRecoverDropsUnfinishedJournalTmp(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	txNoErr(t, os.MkdirAll(stateDir, 0755))
	writeTxnFile(t, filepath.Join(stateDir, "txn-x.json.tmp"), "{broken", 0600)
	writeTxnFile(t, filepath.Join(dir, "a.txt"), "old", 0644)

	reports, err := RecoverTxns(stateDir)
	txNoErr(t, err)
	txEq(t, len(reports), 0)
	matches, err := filepath.Glob(filepath.Join(stateDir, "*"))
	txNoErr(t, err)
	txEq(t, len(matches), 0)
	txEq(t, readTxnFile(t, filepath.Join(dir, "a.txt")), "old")
}

// TestBatchTxnJSONOverrides mirrors the daily export use case: the same set of
// override values is merged into every JSON file and the plan lists the keys
// that change in each file.
func TestBatchTxnJSONOverrides(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")

	fileA := filepath.Join(dir, "a.json")
	fileB := filepath.Join(dir, "b.json")
	writeTxnFile(t, fileA, `{"a":1,"keep":"x"}`, 0644)
	writeTxnFile(t, fileB, `{"a":100,"b":2}`, 0644)

	overrides := map[string]int{"a": 100, "b": 2}
	merge := func(_ string, old []byte) ([]byte, string, error) {
		data := map[string]any{}
		if err := json.Unmarshal(old, &data); err != nil {
			return nil, "", err
		}
		var changedKeys []string
		for k, v := range overrides {
			if cur, ok := data[k]; !ok || cur != v {
				changedKeys = append(changedKeys, k)
				data[k] = v
			}
		}
		if len(changedKeys) == 0 {
			return old, "", nil
		}
		sort.Strings(changedKeys)
		out, err := json.Marshal(data)
		if err != nil {
			return nil, "", err
		}
		return out, "set keys: " + strings.Join(changedKeys, ", "), nil
	}

	tx := NewBatchTxn(WithTxnStateDir(stateDir))
	txNoErr(t, tx.Add(fileA, merge))
	txNoErr(t, tx.Add(fileB, merge))

	plan, err := tx.Plan()
	txNoErr(t, err)
	byPath := map[string]TxnChange{}
	for _, ch := range plan.Changes {
		byPath[ch.Path] = ch
	}
	txTrue(t, byPath[fileA].Changed)
	txEq(t, byPath[fileA].Note, "set keys: a, b")
	txTrue(t, !byPath[fileB].Changed, "file b already holds the override values")

	txNoErr(t, tx.Commit())

	var gotA map[string]any
	txNoErr(t, json.Unmarshal([]byte(readTxnFile(t, fileA)), &gotA))
	txEq(t, gotA["a"], float64(100))
	txEq(t, gotA["b"], float64(2))
	txEq(t, gotA["keep"], "x")
	txEq(t, readTxnFile(t, fileB), `{"a":100,"b":2}`)
	assertNoTxnLeftovers(t, dir)
}

// crashSidecarEntry builds a journal entry with the same sidecar naming rule
// used by the production code.
func crashSidecarEntry(id, path string, perm os.FileMode) *txnFilePlan {
	dir, base := filepath.Split(path)
	return &txnFilePlan{
		Path:    path,
		Tmp:     filepath.Join(dir, "."+base+".fstxnew."+id),
		Bak:     filepath.Join(dir, "."+base+".fstxbak."+id),
		Perm:    uint32(perm.Perm()),
		Changed: true,
	}
}

// writeCrashJournal writes a journal file exactly as Commit would find it
// after a process crash.
func writeCrashJournal(t *testing.T, stateDir string, j *txnJournal) {
	t.Helper()
	data, err := json.MarshalIndent(j, "", "  ")
	txNoErr(t, err)
	txNoErr(t, os.WriteFile(filepath.Join(stateDir, txnJournalPrefix+j.ID+txnJournalSuffix), data, 0600))
}
