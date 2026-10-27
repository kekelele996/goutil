package maskutil_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gookit/goutil/maskutil"
)

// Secret is a named string type.
type Secret string

// SecretBytes is a named []byte type.
type SecretBytes []byte

type Profile struct {
	RealName string
	Phone    string `json:"mobile"`
}

type Req struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Token    Secret `sensitive:""`
	Phone    string `json:"mobile"`
	Age      int
	Pwd      *string `json:"pwd"`
	Profile  *Profile
	Tags     []string
	Friends  []*Profile
	Headers  map[string]string
	Extra    map[string]any
	Any      any
	Created  time.Time
	Bin      []byte
	SecretB  SecretBytes
	ignored  string //nolint:unused
	Inner    struct {
		Password string
		Phone    string
	}
}

func ptr[T any](v T) *T { return &v }

func sampleReq() *Req {
	r := &Req{
		Name:     "tom",
		Password: "p@ssw0rd",
		Token:    "tok_live_123",
		Phone:    "13800001111",
		Age:      18,
		Pwd:      ptr("ptr-secret"),
		Profile: &Profile{
			RealName: "汤姆",
			Phone:    "13900002222",
		},
		Tags: []string{"a", "b"},
		Friends: []*Profile{
			{RealName: "jerry", Phone: "13700003333"},
		},
		Headers: map[string]string{
			"Authorization": "Bearer abc",
			"Content-Type":  "application/json",
		},
		Extra: map[string]any{
			"password": "map-pwd",
			"nested": map[string]any{
				"phone": "13600004444",
			},
			"count": 3,
		},
		Any:     &Profile{RealName: "ani", Phone: "13500005555"},
		Created: time.Date(2026, 10, 27, 12, 30, 0, 0, time.UTC),
		Bin:     []byte("raw-bytes"),
		SecretB: SecretBytes("typed-bytes"),
	}
	r.Inner.Password = "inner-pwd"
	r.Inner.Phone = "inner-phone"
	return r
}

func newTestMasker() *maskutil.Masker {
	return maskutil.New(
		maskutil.WithCommonFields(),
		maskutil.WithMarkTags("sensitive"),
	)
}

func TestMask_BasicStructAndNested(t *testing.T) {
	src := sampleReq()
	m := newTestMasker()

	out, err := m.Mask(src)
	if err != nil {
		t.Fatalf("Mask error: %v", err)
	}

	got, ok := out.(*Req)
	if !ok {
		t.Fatalf("expect *Req, got %T", out)
	}

	if got.Password != maskutil.DefaultPlaceholder {
		t.Errorf("Password = %q", got.Password)
	}
	if string(got.Token) != maskutil.DefaultPlaceholder {
		t.Errorf("Token = %q, and custom type must be kept", got.Token)
	}
	if got.Phone != maskutil.DefaultPlaceholder {
		t.Errorf("Phone = %q (matched by json name mobile)", got.Phone)
	}
	if got.Name != "tom" {
		t.Errorf("Name = %q, should be unchanged", got.Name)
	}
	if got.Age != 18 {
		t.Errorf("Age = %d", got.Age)
	}
	if got.Pwd == nil || *got.Pwd != maskutil.DefaultPlaceholder {
		t.Errorf("Pwd = %v, pointer shape and masked value expected", got.Pwd)
	}
	if got.Profile == nil || got.Profile.RealName != "汤姆" || got.Profile.Phone != maskutil.DefaultPlaceholder {
		t.Errorf("nested Profile not handled: %+v", got.Profile)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "a" || got.Tags[1] != "b" {
		t.Errorf("Tags = %v", got.Tags)
	}
	if len(got.Friends) != 1 || got.Friends[0].RealName != "jerry" || got.Friends[0].Phone != maskutil.DefaultPlaceholder {
		t.Errorf("Friends slice not handled: %+v", got.Friends)
	}
	if got.Headers["Authorization"] != maskutil.DefaultPlaceholder {
		t.Errorf("map Authorization = %q", got.Headers["Authorization"])
	}
	if got.Headers["Content-Type"] != "application/json" {
		t.Errorf("map Content-Type = %q", got.Headers["Content-Type"])
	}
	if got.Extra["password"] != maskutil.DefaultPlaceholder {
		t.Errorf("map any password = %v", got.Extra["password"])
	}
	nested, _ := got.Extra["nested"].(map[string]any)
	if nested["phone"] != maskutil.DefaultPlaceholder {
		t.Errorf("nested map phone = %v", nested["phone"])
	}
	if got.Extra["count"] != 3 {
		t.Errorf("count = %v", got.Extra["count"])
	}
	ap, _ := got.Any.(*Profile)
	if ap == nil || ap.Phone != maskutil.DefaultPlaceholder || ap.RealName != "ani" {
		t.Errorf("interface-held value not handled: %+v", got.Any)
	}
	if got.Inner.Password != maskutil.DefaultPlaceholder || got.Inner.Phone != maskutil.DefaultPlaceholder {
		t.Errorf("anonymous inner struct: %+v", got.Inner)
	}
}

func TestMask_CustomTypesAndBytes(t *testing.T) {
	m := newTestMasker()
	out, err := m.Mask(sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*Req)

	// custom named string type is preserved
	if _, ok := any(got.Token).(Secret); !ok {
		t.Errorf("Token dynamic type = %T, want maskutil_test.Secret", got.Token)
	}
	// []byte non-sensitive value is copied intact
	if string(got.Bin) != "raw-bytes" {
		t.Errorf("Bin = %q", got.Bin)
	}
	// named []byte field is masked via field rule (there is no "secretb"
	// rule, so verify plain []byte fit through a dedicated struct instead)
	type Doc struct {
		Data []byte `json:"data"`
		Pwd  []byte `json:"password"`
	}
	d := &Doc{Data: []byte("keep"), Pwd: []byte("secret")}
	o := m.MustMask(d).(*Doc)
	if string(o.Data) != "keep" {
		t.Errorf("Data = %q", o.Data)
	}
	if string(o.Pwd) != maskutil.DefaultPlaceholder {
		t.Errorf("Pwd bytes = %q", o.Pwd)
	}
}

func TestMask_TimePreserved(t *testing.T) {
	m := newTestMasker()
	got := m.MustMask(sampleReq()).(*Req)

	if got.Created.IsZero() {
		t.Fatal("time.Time must be copied intact, got zero time")
	}
	if !got.Created.Equal(time.Date(2026, 10, 27, 12, 30, 0, 0, time.UTC)) {
		t.Errorf("time changed: %v", got.Created)
	}

	// named alias of time.Time
	type MyTime time.Time
	type Box struct {
		T MyTime
	}
	src := &Box{T: MyTime(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC))}
	o := m.MustMask(src).(*Box)
	if time.Time(o.T).IsZero() {
		t.Error("named time alias became zero")
	}
	if _, ok := any(o.T).(MyTime); !ok {
		t.Errorf("named time type lost: %T", o.T)
	}
}

func TestMask_NonStringFieldFallback(t *testing.T) {
	type Cfg struct {
		Phone    int64
		Password int
		Keep     int
	}
	m := maskutil.New(maskutil.WithFields("phone", "password"))
	got := m.MustMask(Cfg{Phone: 13800001111, Password: 42, Keep: 7}).(Cfg)

	// placeholder string cannot fit int kinds: zero value, no panic/leak
	if got.Phone != 0 || got.Password != 0 {
		t.Errorf("numeric sensitive fields must be zeroed: %+v", got)
	}
	if got.Keep != 7 {
		t.Errorf("Keep = %d", got.Keep)
	}
}

func TestMask_PreserveNilAndShape(t *testing.T) {
	m := newTestMasker()
	r := sampleReq()
	r.Profile = nil
	r.Headers = nil
	r.Friends = nil

	got := m.MustMask(r).(*Req)
	if got.Profile != nil {
		t.Error("nil pointer must stay nil")
	}
	if got.Headers != nil {
		t.Error("nil map must stay nil")
	}
	if got.Friends != nil {
		t.Error("nil slice must stay nil")
	}
}

func TestMask_SourceNotModified(t *testing.T) {
	src := sampleReq()
	m := newTestMasker()

	_, err := m.Mask(src)
	if err != nil {
		t.Fatal(err)
	}

	// every original sensitive value is still there
	if src.Password != "p@ssw0rd" {
		t.Errorf("source Password changed: %q", src.Password)
	}
	if string(src.Token) != "tok_live_123" {
		t.Errorf("source Token changed: %q", src.Token)
	}
	if src.Phone != "13800001111" {
		t.Errorf("source Phone changed: %q", src.Phone)
	}
	if *src.Pwd != "ptr-secret" {
		t.Errorf("source Pwd changed: %q", *src.Pwd)
	}
	if src.Profile.Phone != "13900002222" {
		t.Errorf("source nested Phone changed: %q", src.Profile.Phone)
	}
	if src.Headers["Authorization"] != "Bearer abc" {
		t.Errorf("source map changed: %q", src.Headers["Authorization"])
	}
	if src.Extra["password"] != "map-pwd" {
		t.Errorf("source any-map changed: %v", src.Extra["password"])
	}
	if string(src.Bin) != "raw-bytes" {
		t.Errorf("source bytes changed: %q", src.Bin)
	}
	if string(src.SecretB) != "typed-bytes" {
		t.Errorf("source typed bytes changed: %q", src.SecretB)
	}

	// masking returns a separate object: mutating the copy must not affect src
	got := m.MustMask(sampleReq()).(*Req)
	got.Tags[0] = "MUTATED"
	got.Headers["Content-Type"] = "x"
	got.Profile.RealName = "X"
	if src.Tags[0] == "MUTATED" || src.Headers["Content-Type"] == "x" || src.Profile.RealName == "X" {
		t.Error("copy shares mutable memory with the source object")
	}
}

func TestMask_CustomPlaceholderAndRule(t *testing.T) {
	type V struct {
		Password string
		Phone    string
	}
	m := maskutil.New(
		maskutil.WithPlaceholder("##"),
		maskutil.WithFields("password"),
		maskutil.WithRule("phone", "1**********"),
	)
	got := m.MustMask(V{Password: "p", Phone: "13800001111"}).(V)

	if got.Password != "##" {
		t.Errorf("Password = %q", got.Password)
	}
	if got.Phone != "1**********" {
		t.Errorf("Phone = %q", got.Phone)
	}
}

func TestMask_MarkTagRuleAndOptOut(t *testing.T) {
	type V struct {
		A string `sensitive:""`
		B string `sensitive:"phone"`
		C string `sensitive:"-"`
		D string `sensitive:"-"`
	}
	m := maskutil.New(
		maskutil.WithMarkTags("sensitive"),
		maskutil.WithRule("phone", "PH"),
		maskutil.WithFields("d"),
	)
	got := m.MustMask(V{A: "a", B: "b", C: "c", D: "d"}).(V)

	if got.A != maskutil.DefaultPlaceholder {
		t.Errorf("A = %q", got.A)
	}
	if got.B != "PH" {
		t.Errorf("B = %q, mark tag value should select rule", got.B)
	}
	if got.C != "c" {
		t.Errorf("C = %q, tag value \"-\" should opt out", got.C)
	}
	if got.D != maskutil.DefaultPlaceholder {
		t.Errorf("D = %q, field name rule still applies", got.D)
	}
}

func TestMask_CustomMaskFunc(t *testing.T) {
	type V struct {
		Inner struct {
			Phone string
		}
		Note string
	}
	v := V{Note: "x"}
	v.Inner.Phone = "13800001111"

	var paths []string
	m := maskutil.New(maskutil.WithMaskFunc(func(path, name string, rv reflect.Value) (any, bool) {
		paths = append(paths, path)
		if path == "Inner.Phone" {
			return "CUSTOM", true
		}
		return nil, false
	}))

	got := m.MustMask(v).(V)
	if got.Inner.Phone != "CUSTOM" {
		t.Errorf("custom func not applied: %q", got.Inner.Phone)
	}
	if got.Note != "x" {
		t.Errorf("Note = %q", got.Note)
	}

	found := false
	for _, p := range paths {
		if p == "Inner.Phone" {
			found = true
		}
	}
	if !found {
		t.Errorf("custom func never received path Inner.Phone, got %v", paths)
	}
}

// cyclic linked list
type node struct {
	Name string
	Next *node
}

type cyclicMap map[string]any

type cyclicSlice []any

func TestMask_CyclicReferences(t *testing.T) {
	// pointer cycle: n1 -> n2 -> n1
	n1 := &node{Name: "n1"}
	n2 := &node{Name: "n2"}
	n1.Next = n2
	n2.Next = n1

	m := maskutil.New()
	o1 := m.MustMask(n1).(*node)

	if o1.Name != "n1" || o1.Next == nil || o1.Next.Name != "n2" {
		t.Fatalf("first-level shape wrong: %+v", o1)
	}
	if o1.Next.Next != nil {
		t.Errorf("cyclic pointer must terminate as nil, got %+v", o1.Next.Next)
	}

	// map cycle
	cm := cyclicMap{"name": "m"}
	cm["self"] = cm
	o2 := m.MustMask(cm).(cyclicMap)
	if o2["name"] != "m" {
		t.Errorf("map value = %v", o2["name"])
	}
	if o2["self"] != nil {
		t.Errorf("cyclic map ref must terminate as nil, got %v", o2["self"])
	}

	// slice cycle: cs[0] is cs itself
	cs := cyclicSlice{nil}
	cs[0] = cs
	o3 := m.MustMask(cs).(cyclicSlice)
	if len(o3) != 1 || o3[0] != nil {
		t.Errorf("cyclic slice ref must terminate as nil, got %v", o3)
	}
	if bts3, err := m.JSON(cs); err != nil || string(bts3) != "[null]" {
		t.Errorf("cyclic slice json = %s, %v", bts3, err)
	}

	// JSON entry must succeed on cyclic data
	bts, err := m.JSON(n1)
	if err != nil {
		t.Fatalf("JSON on cyclic data must terminate, got error: %v", err)
	}
	if !strings.Contains(string(bts), `"Name":"n2"`) {
		t.Errorf("json content unexpected: %s", bts)
	}
}

func TestMask_OutputEntriesConsistent(t *testing.T) {
	src := sampleReq()
	m := newTestMasker()

	masked, err := m.Mask(src)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(masked)
	if err != nil {
		t.Fatal(err)
	}

	gotJSON, err := m.JSON(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("JSON() differs from json.Marshal(Mask()):\n got %s\nwant %s", gotJSON, wantJSON)
	}

	// repeated runs on the same source are deterministic
	again := m.MustMask(sampleReq()).(*Req)
	first := masked.(*Req)
	b1, _ := json.Marshal(first)
	b2, _ := json.Marshal(again)
	if string(b1) != string(b2) {
		t.Errorf("non-deterministic output:\n%s\n%s", b1, b2)
	}

	s := m.String(src)
	if !strings.Contains(s, maskutil.DefaultPlaceholder) {
		t.Errorf("String output should contain placeholder: %s", s)
	}
	if strings.Contains(s, "p@ssw0rd") || strings.Contains(s, "13800001111") {
		t.Errorf("String output leaks sensitive data: %s", s)
	}
}

func TestMask_JSONDoesNotLeak(t *testing.T) {
	bts, err := newTestMasker().JSON(sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	s := string(bts)
	for _, leak := range []string{"p@ssw0rd", "tok_live_123", "13800001111", "13900002222", "13700003333", "Bearer abc", "map-pwd", "13600004444", "13500005555", "inner-pwd"} {
		if strings.Contains(s, leak) {
			t.Errorf("json leaks %q: %s", leak, s)
		}
	}
	if !strings.Contains(s, `"`+maskutil.DefaultPlaceholder+`"`) {
		t.Errorf("json should contain placeholder: %s", s)
	}
}

func TestMask_DefaultPreset(t *testing.T) {
	type V struct {
		Password string
		Token    string
		Phone    string
		Name     string
	}
	got := maskutil.Default().MustMask(V{Password: "p", Token: "t", Phone: "138", Name: "n"}).(V)
	if got.Password != maskutil.DefaultPlaceholder || got.Token != maskutil.DefaultPlaceholder ||
		got.Phone != maskutil.DefaultPlaceholder || got.Name != "n" {
		t.Errorf("default preset mismatch: %+v", got)
	}
}

func TestMask_NilAndScalars(t *testing.T) {
	m := maskutil.New(maskutil.WithFields("password"))

	out, err := m.Mask(nil)
	if err != nil || out != nil {
		t.Errorf("nil input: %v, %v", out, err)
	}
	if m.MustMask("hello") != "hello" {
		t.Error("scalar string should be returned as-is")
	}
	if m.MustMask(123) != 123 {
		t.Error("scalar int should be returned as-is")
	}

	// map with non-string keys is still copied recursively
	src := map[int]string{1: "a"}
	o := m.MustMask(src).(map[int]string)
	if o[1] != "a" {
		t.Errorf("int-keyed map = %v", o)
	}
}

func TestMask_ArrayAndPointerChains(t *testing.T) {
	type V struct {
		Password string
	}
	type Wrap struct {
		Items [2]V
		Next  **V
	}
	inner := &V{Password: "deep"}
	w := &Wrap{Next: &inner}
	w.Items[0] = V{Password: "p0"}
	w.Items[1] = V{Password: "p1"}

	m := maskutil.New(maskutil.WithFields("password"))
	got := m.MustMask(w).(*Wrap)
	if got.Items[0].Password != maskutil.DefaultPlaceholder || got.Items[1].Password != maskutil.DefaultPlaceholder {
		t.Errorf("array elements: %+v", got.Items)
	}
	if got.Next == nil || *got.Next == nil || (**got.Next).Password != maskutil.DefaultPlaceholder {
		t.Errorf("pointer chain not handled: %+v", got.Next)
	}
}

func TestMask_ConcurrentUse(t *testing.T) {
	m := newTestMasker()
	src := sampleReq()

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 20; j++ {
				got := m.MustMask(src).(*Req)
				if got.Password != maskutil.DefaultPlaceholder {
					t.Errorf("Password = %q", got.Password)
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

func ExampleNew() {
	type LoginReq struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}

	m := maskutil.New(
		maskutil.WithFields("password"),
		maskutil.WithMarkTags("sensitive"),
	)

	bts, _ := m.JSON(LoginReq{User: "tom", Password: "secret"})
	fmt.Println(string(bts))
	// Output: {"user":"tom","password":"******"}
}
