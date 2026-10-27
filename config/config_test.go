package config

import (
	"os"
	"strings"
	"testing"
)

type testConf struct {
	AppName string  `config:"app_name"`
	Port    int     `config:"port"`
	Debug   bool    `config:"debug"`
	Ratio   float64 `config:"ratio"`
	DB      struct {
		Host string `config:"host"`
		Port int    `config:"port"`
	} `config:"db"`
	Tags []string `config:"tags"`
}

func layersCommonDev() []Layer {
	return []Layer{
		FileLayer("common", "testdata/base.json"),
		FileLayer("dev", "testdata/dev.env"),
	}
}

func TestLoadOrderedMergeAndProvenance(t *testing.T) {
	var cfg testConf
	rpt, err := LoadInto(&cfg, layersCommonDev())
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	// later layer wins for overrides, earlier layer survives for the rest
	if cfg.AppName != "demo" {
		t.Errorf("AppName = %q, want demo", cfg.AppName)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090 from dev", cfg.Port)
	}
	if !cfg.Debug {
		t.Errorf("Debug = false, want true from dev")
	}
	if cfg.Ratio != 1.5 {
		t.Errorf("Ratio = %v, want 1.5 from common", cfg.Ratio)
	}
	if cfg.DB.Host != "db.dev.local" {
		t.Errorf("DB.Host = %q, want db.dev.local from dev", cfg.DB.Host)
	}
	if cfg.DB.Port != 5432 {
		t.Errorf("DB.Port = %d, want 5432 from common (nested merge)", cfg.DB.Port)
	}
	if len(cfg.Tags) != 2 || cfg.Tags[0] != "base" {
		t.Errorf("Tags = %v, want [base common]", cfg.Tags)
	}

	// every final value can be traced back to its winning layer
	cases := map[string]string{
		"app_name": "common",
		"port":     "dev",
		"debug":    "dev",
		"ratio":    "common",
		"db.host":  "dev",
		"db.port":  "common",
	}
	for setting, want := range cases {
		if got, ok := rpt.SourceOf(setting); !ok || got != want {
			t.Errorf("SourceOf(%q) = %q,%v want %q", setting, got, ok, want)
		}
	}

	// overridden keys are listed by name with the adopted layer
	dups := rpt.Duplicates()
	dupWinner := map[string]string{}
	for _, is := range dups {
		dupWinner[is.Setting] = is.Winner
		if len(is.Layers) != 2 || is.Layers[0] != "common" || is.Layers[1] != "dev" {
			t.Errorf("duplicate %q layers = %v, want [common dev]", is.Setting, is.Layers)
		}
	}
	for _, key := range []string{"port", "debug", "db.host"} {
		if dupWinner[key] != "dev" {
			t.Errorf("duplicate %q winner = %q, want dev", key, dupWinner[key])
		}
	}
	if _, ok := dupWinner["app_name"]; ok {
		t.Errorf("app_name defined once must not be reported as duplicate")
	}
}

func TestLoadNeverTouchesProcessEnv(t *testing.T) {
	const key = "CONFIG_TEST_PORT"
	if err := os.Setenv(key, "orig"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv(key)

	var cfg struct {
		Port int `config:"CONFIG_TEST_PORT"`
	}
	_, err := LoadInto(&cfg, []Layer{
		{Name: "envfile", Format: FormatKV, Text: "CONFIG_TEST_PORT=9090\n"},
	})
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if got := os.Getenv(key); got != "orig" {
		t.Errorf("process env was modified: %s=%q, want orig", key, got)
	}
}

func TestTypeErrorsReportedByNameWithLayer(t *testing.T) {
	var cfg testConf
	// pre-existing user values must remain untouched on failure
	cfg.AppName = "keep-me"

	_, err := LoadInto(&cfg, []Layer{
		FileLayer("common", "testdata/base.json"),
		FileLayer("bad", "testdata/bad.env"),
	})
	le := asLoadError(t, err, "decode")

	bad := map[string]string{} // setting -> want type
	for _, is := range le.Issues {
		if is.Kind != IssueTypeError {
			t.Fatalf("unexpected issue kind %s: %+v", is.Kind, is)
		}
		bad[is.Setting] = is.Want
		if is.Winner != "bad" {
			t.Errorf("%s winner = %q, want bad", is.Setting, is.Winner)
		}
	}
	if bad["port"] != "int" {
		t.Errorf("port type issue want int, got %q", bad["port"])
	}
	if bad["debug"] != "bool" {
		t.Errorf("debug type issue want bool, got %q", bad["debug"])
	}
	if bad["db.port"] != "int" {
		t.Errorf("db.port type issue want int, got %q", bad["db.port"])
	}
	if !strings.Contains(err.Error(), `"port"`) || !strings.Contains(err.Error(), `"db.port"`) {
		t.Errorf("error must name bad settings: %v", err)
	}

	// nothing half-applied: valid keys did not land either
	if cfg.Port != 0 {
		t.Errorf("Port = %d, want 0 after failed load", cfg.Port)
	}
	if cfg.AppName != "keep-me" {
		t.Errorf("AppName = %q, want keep-me, target must not change on failure", cfg.AppName)
	}
}

// writeTempLayers copies testdata files (name, src pairs, in merge order)
// into a temp dir and returns layers pointing at them. Tests mutate these
// files between Reload calls.
func writeTempLayers(t *testing.T, pairs ...string) []Layer {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatal("writeTempLayers needs name/src pairs")
	}
	dir := t.TempDir()
	layers := make([]Layer, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		name, src := pairs[i], pairs[i+1]
		path := dir + "/" + name
		if err := os.WriteFile(path, mustReadFile(t, src), 0o644); err != nil {
			t.Fatal(err)
		}
		layers = append(layers, FileLayer(stripExt(name), path))
	}
	return layers
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func stripExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

func TestFailedReloadKeepsPreviousGeneration(t *testing.T) {
	layers := writeTempLayers(t, "base.json", "testdata/base.json", "dev.env", "testdata/dev.env")

	var cfg testConf
	l, err := New(&cfg, layers)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Load(); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	old := l.Snapshot()

	// files on disk change to a generation with mistyped values
	if err = os.WriteFile(layers[1].Path, mustReadFile(t, "testdata/bad.env"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = l.Reload(); err == nil {
		t.Fatal("reload must fail with bad layers")
	}

	// the usable generation survives intact, nothing is half-applied
	if cfg.Port != 9090 || cfg.AppName != "demo" || cfg.DB.Host != "db.dev.local" {
		t.Errorf("target was half-replaced: %+v", cfg)
	}
	if cur := l.Snapshot(); cur != old {
		t.Error("snapshot changed after failed reload")
	}
	if oldCfg, ok := old.Config().(*testConf); !ok || oldCfg.Port != 9090 {
		t.Error("old snapshot no longer holds the usable values")
	}
}

func TestReloadReplacesFullyWithoutStaleValues(t *testing.T) {
	layers := writeTempLayers(t, "base.json", "testdata/base.json", "dev.env", "testdata/dev.env")

	var cfg testConf
	l, err := New(&cfg, layers)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Load(); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	// new generation on disk no longer defines app_name/db.host/tags
	newBase := `{
  "port": 80,
  "debug": false,
  "db": {"port": 5433}
}
`
	if err = os.WriteFile(layers[0].Path, []byte(newBase), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(layers[1].Path, []byte("# nothing overridden\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = l.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if cfg.Port != 80 || cfg.Debug {
		t.Errorf("new values not applied: port=%d debug=%v", cfg.Port, cfg.Debug)
	}
	if cfg.AppName != "" {
		t.Errorf("stale AppName = %q, want empty after full replacement", cfg.AppName)
	}
	if cfg.DB.Host != "" {
		t.Errorf("stale DB.Host = %q, want empty", cfg.DB.Host)
	}
	if cfg.DB.Port != 5433 {
		t.Errorf("DB.Port = %d, want 5433", cfg.DB.Port)
	}
	if cfg.Tags != nil {
		t.Errorf("stale Tags = %v, want nil", cfg.Tags)
	}
	if got, _ := l.Report().SourceOf("port"); got != "base" {
		t.Errorf("SourceOf(port) = %q, want base", got)
	}
}

func TestDefaultsAndNoSilentFallback(t *testing.T) {
	var cfg testConf
	defs := testConf{AppName: "default-app", Port: 7000}
	defs.Tags = []string{"d1"}

	// missing keys keep defaults
	l, err := New(&cfg, []Layer{FileLayer("prod", "testdata/prod.properties")}, WithDefaults(defs))
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.AppName != "default-app" || cfg.Port != 80 || len(cfg.Tags) != 1 || cfg.Tags[0] != "d1" {
		t.Errorf("defaults/overrides wrong: %+v", cfg)
	}

	// a mistyped key errors instead of silently keeping the default,
	// and the target keeps its current usable values
	var cfg2 testConf
	cfg2.Port = 7000
	layers := []Layer{
		FileLayer("common", "testdata/base.json"),
		FileLayer("bad", "testdata/bad.env"),
	}
	l2, err := New(&cfg2, layers, WithDefaults(defs))
	if err != nil {
		t.Fatal(err)
	}
	if err = l2.Load(); err == nil {
		t.Fatal("want type error")
	}
	if cfg2.Port != 7000 {
		t.Errorf("Port = %d, want 7000, usable config must not change", cfg2.Port)
	}
	if l2.Loaded() {
		t.Error("no snapshot may exist after failed initial load")
	}
}

func TestMergeShapeConflict(t *testing.T) {
	var cfg testConf
	_, err := LoadInto(&cfg, []Layer{
		FileLayer("common", "testdata/base.json"),
		FileLayer("conflict", "testdata/conflict.env"),
	})
	le := asLoadError(t, err, "merge")
	if len(le.Issues) != 1 || le.Issues[0].Setting != "db" || le.Issues[0].Winner != "conflict" {
		t.Fatalf("want one db conflict from conflict layer, got %+v", le.Issues)
	}
}

func TestUnknownKeysReportedAndOptionallyFatal(t *testing.T) {
	layers := []Layer{
		{Name: "common", Format: FormatJSON, Text: `{"port": 8080, "weird": 1}`},
		{Name: "dev", Format: FormatKV, Text: "other.key=x\n"},
	}
	var cfg struct {
		Port int `config:"port"`
	}
	rpt, err := LoadInto(&cfg, layers)
	if err != nil {
		t.Fatalf("unknown keys are non-fatal by default: %v", err)
	}
	if got, _ := rpt.SourceOf("weird"); got != "common" {
		t.Errorf("SourceOf(weird) = %q, want common", got)
	}
	if got, _ := rpt.SourceOf("other.key"); got != "dev" {
		t.Errorf("SourceOf(other.key) = %q, want dev", got)
	}

	var cfg2 struct {
		Port int `config:"port"`
	}
	if _, err = LoadInto(&cfg2, layers, WithRejectUnknown()); err == nil {
		t.Fatal("reject-unknown must make unknown keys fatal")
	}
}

func TestOptionalMissingLayer(t *testing.T) {
	layers := []Layer{
		FileLayer("common", "testdata/base.json"),
		{Name: "maybe-dev", Path: "testdata/does-not-exist.env", Optional: true},
	}
	var cfg testConf
	rpt, err := LoadInto(&cfg, layers)
	if err != nil {
		t.Fatalf("optional missing layer: %v", err)
	}
	if len(rpt.Skipped) != 1 || rpt.Skipped[0] != "maybe-dev" {
		t.Errorf("Skipped = %v, want [maybe-dev]", rpt.Skipped)
	}

	var cfg2 testConf
	_, err = LoadInto(&cfg2, []Layer{
		{Name: "required", Path: "testdata/does-not-exist.env"},
	})
	if err == nil {
		t.Fatal("missing required layer must error")
	}
}

func TestStrictCoercions(t *testing.T) {
	type types struct {
		N   int8   `config:"n"`
		U   uint16 `config:"u"`
		F   float32
		B   bool
		IDs []int `config:"ids"`
	}

	// comma separated KV list and basic conversions
	var ok types
	_, err := LoadInto(&ok, []Layer{{Name: "kv", Format: FormatKV, Text: "n=12\nu=65535\nF=2.5\nB=1\nids=1, 2 ,3\n"}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ok.N != 12 || ok.U != 65535 || ok.F != 2.5 || !ok.B || len(ok.IDs) != 3 || ok.IDs[1] != 2 {
		t.Errorf("coercions wrong: %+v", ok)
	}

	// int8 overflow and bad element are reported by name
	var bad types
	err = LoadIntoOrErr(&bad, []Layer{{Name: "json", Format: FormatJSON, Text: `{"n": 128, "ids": [1, "x"]}`}})
	le := asLoadError(t, err, "decode")
	names := map[string]bool{}
	for _, is := range le.Issues {
		names[is.Setting] = true
	}
	if !names["n"] || !names["ids[1]"] {
		t.Errorf("want issues for n and ids[1], got %v", names)
	}

	// large integer keeps precision through json.Number
	var big struct {
		V int64 `config:"v"`
	}
	_, err = LoadInto(&big, []Layer{{Name: "json", Format: FormatJSON, Text: `{"v": 9007199254740993}`}})
	if err != nil {
		t.Fatalf("big int load: %v", err)
	}
	if big.V != 9007199254740993 {
		t.Errorf("V = %d, precision lost", big.V)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(123, layersCommonDev()); err == nil {
		t.Error("non-pointer target must fail")
	}
	var n int
	if _, err := New(&n, layersCommonDev()); err == nil {
		t.Error("pointer to non-struct must fail")
	}
	var cfg testConf
	if _, err := New(&cfg, nil); err == nil {
		t.Error("empty layers must fail")
	}
	if _, err := New(&cfg, []Layer{
		{Name: "x", Format: FormatKV, Text: "a=1\n"},
		{Name: "x", Format: FormatKV, Text: "b=2\n"},
	}); err == nil {
		t.Error("duplicate layer names must fail")
	}
}

// --- small test helpers ---

func asLoadError(t *testing.T, err error, stage string) *LoadError {
	t.Helper()
	if err == nil {
		t.Fatalf("want LoadError at stage %s, got nil", stage)
	}
	le, ok := err.(*LoadError)
	if !ok {
		t.Fatalf("want *LoadError, got %T: %v", err, err)
	}
	if le.Stage != stage {
		t.Fatalf("stage = %q, want %q", le.Stage, stage)
	}
	return le
}

// LoadIntoOrErr is only used to keep call sites symmetric in tests.
func LoadIntoOrErr(target any, layers []Layer, opts ...Option) error {
	_, err := LoadInto(target, layers, opts...)
	return err
}
