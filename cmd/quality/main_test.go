// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const module = "github.com/IceSkatingCoach/MyMicroTunnel"

const goTestJSON = `{"Action":"start","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup"}
{"Action":"run","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestPlain"}
{"Action":"pass","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestPlain","Elapsed":0.25}
{"Action":"run","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestTable"}
{"Action":"run","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestTable/one"}
{"Action":"pass","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestTable/one","Elapsed":0.5}
{"Action":"run","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestTable/two"}
{"Action":"output","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestTable/two","Output":"=== RUN   TestTable/two\n"}
{"Action":"output","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestTable/two","Output":"    setup_test.go:40: wanted 1 at /home/runner/work/MyMicroTunnel/MyMicroTunnel/internal/setup/setup.go:9\n"}
{"Action":"output","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestTable/two","Output":"goroutine stack that must not be published\n"}
{"Action":"fail","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestTable/two","Elapsed":0}
{"Action":"fail","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestTable","Elapsed":0.5}
{"Action":"skip","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Test":"TestSkipped","Elapsed":0}
{"Action":"fail","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup","Elapsed":1}
{"Action":"output","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/broken","Output":"# github.com/IceSkatingCoach/MyMicroTunnel/internal/broken\n"}
{"Action":"fail","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/broken","Elapsed":0}
{"Action":"skip","Package":"github.com/IceSkatingCoach/MyMicroTunnel/internal/notests","Elapsed":0}
{"Action":"run","Package":"github.com/IceSkatingCoach/MyMicroTunnel","Test":"TestRoot"}
{"Action":"pass","Package":"github.com/IceSkatingCoach/MyMicroTunnel","Test":"TestRoot","Elapsed":0}
`

const coverProfile = `mode: atomic
github.com/IceSkatingCoach/MyMicroTunnel/internal/setup/a.go:1.1,2.1 6 1
github.com/IceSkatingCoach/MyMicroTunnel/internal/setup/a.go:3.1,4.1 4 0
github.com/IceSkatingCoach/MyMicroTunnel/internal/setup/a.go:3.1,4.1 4 2
github.com/IceSkatingCoach/MyMicroTunnel/internal/setup/b.go:1.1,2.1 10 0
not a block
github.com/x/y.go:1.1,2.1 a b
nocolon 1 1
`

// govulncheck writes indented JSON objects one after another.
const vulnJSON = `{"config":{"protocol_version":"v1.0.0"}}
{"osv":{"id":"GO-1"}}
{"finding":{"osv":"GO-1","trace":[{"module":"m","function":"Called"}]}}
{"finding":{"osv":"GO-1","trace":[{"module":"m","function":"AlsoCalled"}]}}
{"finding":{"osv":"GO-2","trace":[{"module":"m","package":"p"}]}}
{"finding":{"osv":"GO-3","trace":[]}}
`

var testContext = Context{ID: "1-1", Time: "2026-10-03T00:00:00.000Z", Branch: "main", Commit: "abc1234", Workflow: "CI"}

func float(v float64) *float64 { return &v }

func TestParseGoTestCountsLeafTestsAndKeepsOneLineOfAFailure(t *testing.T) {
	suites, failed, err := parseGoTest(strings.NewReader(goTestJSON), "macos", module)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Suite{}
	for _, s := range suites {
		got[s.Name] = s
	}
	// TestTable is a grouping of its two subtests, not a third test.
	if s := got["internal/setup"]; s.Tests != (Tests{Total: 4, Passed: 2, Failed: 1, Skipped: 1}) || s.DurationMs != 750 {
		t.Errorf("internal/setup = %+v", s)
	}
	if s := got["internal/broken"]; s.Tests != (Tests{Total: 1, Failed: 1}) {
		t.Errorf("a package that failed to build must count as a failure, got %+v", s)
	}
	if _, ok := got["internal/notests"]; ok {
		t.Error("a package with no tests is not a suite")
	}
	if s := got["."]; s.Passed != 1 {
		t.Errorf("the module root should be named \".\", got %+v", got)
	}
	if len(failed) != 2 {
		t.Fatalf("failed = %+v", failed)
	}
	if failed[0].Name != "TestTable/two" || failed[0].Message != "setup_test.go:40: wanted 1 at setup.go:9" {
		t.Errorf("failure = %+v", failed[0])
	}
	if failed[1].Name != "(package failed to build or run)" {
		t.Errorf("failure = %+v", failed[1])
	}
	if strings.Contains(fmt.Sprint(failed), "goroutine stack") {
		t.Error("a stack trace reached the published failures")
	}
}

func TestParseGoTestRejectsOutputThatIsNotJSON(t *testing.T) {
	if _, _, err := parseGoTest(strings.NewReader("FAIL\n"), "macos", module); err == nil {
		t.Error("expected an error")
	}
}

func TestFirstLineAndFailureMessage(t *testing.T) {
	if got := firstLine("\n  x at /a/b/c.go:3\nmore"); got != "x at c.go:3" {
		t.Errorf("firstLine = %q", got)
	}
	if got := len(firstLine(strings.Repeat("x", 500))); got != 300 {
		t.Errorf("length = %d", got)
	}
	if firstLine("") != "" || failureMessage([]string{"--- FAIL: T", "FAIL", "FAIL\tpkg 0.1s", "PASS"}) != "" {
		t.Error("bookkeeping lines are not a message")
	}
}

func TestParseCoverProfileCountsStatementsOnceEach(t *testing.T) {
	files, totals := parseCoverProfile(coverProfile, "macos", module)
	if totals != (statementTotals{found: 20, hit: 10}) {
		t.Errorf("totals = %+v", totals)
	}
	if len(files) != 2 || files[0].File != "internal/setup/a.go" || *files[0].Lines != 100 || *files[1].Lines != 0 {
		t.Errorf("files = %+v", files)
	}
	// Go measures neither branches nor functions; no data is not 0%.
	if c := coverageFromTotals(totals); *c.Lines != 50 || *c.Statements != 50 || c.Branches != nil || c.Functions != nil {
		t.Errorf("coverage = %+v", c)
	}
	if c := coverageFromTotals(statementTotals{}); c.Lines != nil {
		t.Error("an empty profile is no data")
	}
}

func TestVulnCountsCountsEachCalledVulnerabilityOnce(t *testing.T) {
	got, err := vulnCounts(strings.NewReader(vulnJSON))
	if err != nil || got != (Security{High: 1}) {
		t.Errorf("got %+v, %v", got, err)
	}
	if _, err := vulnCounts(strings.NewReader("network down")); err == nil {
		t.Error("expected an error")
	}
}

func TestBuildContextReadsGitHubActionsVariables(t *testing.T) {
	env := map[string]string{
		"GITHUB_REPOSITORY": "o/r", "GITHUB_SHA": "sha1", "GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "2",
		"GITHUB_REF_NAME": "3/merge", "GITHUB_HEAD_REF": "fix/x", "GITHUB_WORKFLOW": "CI",
	}
	c := buildContext(func(k string) string { return env[k] }, time.Date(2026, 10, 3, 1, 2, 3, 0, time.UTC))
	if c.Branch != "fix/x" || c.ID != "42-2" || c.Time != "2026-10-03T01:02:03.000Z" ||
		*c.CommitURL != "https://github.com/o/r/commit/sha1" || *c.BuildURL != "https://github.com/o/r/actions/runs/42" {
		t.Errorf("context = %+v", c)
	}
	local := buildContext(func(string) string { return "" }, time.UnixMilli(7))
	if local.Branch != "local" || local.ID != "7-1" || local.CommitURL != nil || local.BuildURL != nil || local.Workflow != "local" {
		t.Errorf("local = %+v", local)
	}
}

func fixtureDir(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// linuxCover shares a.go's first block with coverProfile but ran the one
// macOS left uncovered, and has a file of its own.
const linuxCover = `mode: atomic
github.com/IceSkatingCoach/MyMicroTunnel/internal/setup/a.go:1.1,2.1 6 1
github.com/IceSkatingCoach/MyMicroTunnel/internal/setup/b.go:1.1,2.1 10 1
github.com/IceSkatingCoach/MyMicroTunnel/internal/sys/x_linux.go:1.1,2.1 5 0
`

func TestCollectMergesBothPlatformsAndCountsSharedCodeOnce(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"macos-test.json": goTestJSON, "macos.cover": coverProfile,
		"linux-test.json": `{"Action":"pass","Package":"` + module + `","Test":"TestLinux"}`, "linux.cover": linuxCover,
		vulnReport: vulnJSON,
	})
	run, err := collect(dir, module, testContext)
	if err != nil {
		t.Fatal(err)
	}
	s := run.Summary
	if s.Status != "failed" || s.Tests != (Tests{Total: 7, Passed: 4, Failed: 2, Skipped: 1}) {
		t.Errorf("summary = %+v", s)
	}
	if *s.Coverage["macos"].Lines != 50 || *s.Coverage["linux"].Lines != 76.2 || s.Security.High != 1 || len(run.Files) != 5 {
		t.Errorf("summary = %+v, %d files", s, len(run.Files))
	}
	// a.go 10/10, b.go 10/10 (Linux ran it), x_linux.go 0/5.
	if *s.Overall.Lines != 80 {
		t.Errorf("overall = %v", *s.Overall.Lines)
	}
}

func TestCollectFailsAPlatformThatLeftNoTestReport(t *testing.T) {
	run, err := collect(fixtureDir(t, map[string]string{"macos-test.json": `{"Action":"pass","Package":"m","Test":"T"}`}), module, testContext)
	if err != nil {
		t.Fatal(err)
	}
	if run.Summary.Status != "failed" || len(run.Failed) != 1 || run.Failed[0].Name != "linux tests did not produce a report" || run.Summary.Overall.Lines != nil {
		t.Errorf("run = %+v", run)
	}
}

func TestCollectPassesACleanRunAndIgnoresAnUnreadableAudit(t *testing.T) {
	pass := `{"Action":"pass","Package":"m","Test":"T"}`
	dir := fixtureDir(t, map[string]string{"macos-test.json": pass, "linux-test.json": pass, vulnReport: "offline"})
	run, err := collect(dir, module, testContext)
	if err != nil || run.Summary.Status != "passed" || run.Summary.Security != (Security{}) {
		t.Errorf("run = %+v, %v", run, err)
	}
	if _, err := collect(fixtureDir(t, map[string]string{"macos-test.json": "nope"}), module, testContext); err == nil {
		t.Error("expected an error for an unreadable test report")
	}
}

func summary(change func(*RunSummary)) RunSummary {
	s := RunSummary{
		Context: testContext, Status: "passed", Tests: Tests{Total: 10, Passed: 10},
		Coverage: map[string]Coverage{"go": {Lines: float(90)}}, Overall: Coverage{Lines: float(90)},
	}
	if change != nil {
		change(&s)
	}
	return s
}

func TestFindRegressionsComparesWithTheLastPassingDefaultBranchBuildOnly(t *testing.T) {
	history := &History{Runs: []RunSummary{
		summary(func(s *RunSummary) { s.ID = "a"; s.Coverage = map[string]Coverage{"go": {Lines: float(95)}} }),
		summary(func(s *RunSummary) {
			s.ID = "b"
			s.Status = "failed"
			s.Coverage = map[string]Coverage{"go": {Lines: float(99)}}
		}),
		summary(func(s *RunSummary) {
			s.ID = "c"
			s.Branch = "feature"
			s.Coverage = map[string]Coverage{"go": {Lines: float(99)}}
		}),
		summary(func(s *RunSummary) { s.ID = "d"; s.Workflow = "Other"; s.Tests.Total = 1 }),
	}}
	if got := findRegressions(summary(nil), history); fmt.Sprint(got) != "[go lines coverage fell from 95% to 90%]" {
		t.Errorf("got %v", got)
	}
	if got := findRegressions(summary(func(s *RunSummary) { s.Coverage["go"] = Coverage{Lines: float(94.6)} }), history); len(got) != 0 {
		t.Errorf("within tolerance, got %v", got)
	}
	if got := findRegressions(summary(func(s *RunSummary) { s.Coverage = map[string]Coverage{"other": {Lines: float(1)}} }), history); len(got) != 0 {
		t.Errorf("an area the baseline lacks, got %v", got)
	}
	if got := findRegressions(summary(func(s *RunSummary) { s.Tests.Total = 5; s.Coverage = nil }), history); fmt.Sprint(got) != "[test count fell from 10 to 5]" {
		t.Errorf("got %v", got)
	}
	if findRegressions(summary(nil), nil) != nil || findRegressions(summary(nil), &History{}) != nil {
		t.Error("no history means no baseline")
	}
}

func TestAppendRunReplacesARerunAndCapsTheHistory(t *testing.T) {
	history := &History{}
	for i := 0; i < historyLimit; i++ {
		history.Runs = append(history.Runs, summary(func(s *RunSummary) { s.ID = fmt.Sprintf("r%d", i) }))
	}
	next := appendRun(history, summary(func(s *RunSummary) { s.ID = "r999"; s.Status = "failed" }))
	if len(next.Runs) != historyLimit || next.Runs[historyLimit-1].Status != "failed" || next.DefaultBranch != "main" || next.Product.Slug != productSlug {
		t.Errorf("history = %d runs, last %+v", len(next.Runs), next.Runs[len(next.Runs)-1])
	}
	if len(appendRun(history, summary(func(s *RunSummary) { s.ID = "new" })).Runs) != historyLimit {
		t.Error("the history was not capped")
	}
	if len(appendRun(nil, summary(nil)).Runs) != 1 {
		t.Error("a first run starts the history")
	}
}

func TestMarkdownSummary(t *testing.T) {
	failing := Run{Summary: summary(func(s *RunSummary) { s.Status = "failed" }), Failed: []Failure{{Area: "go", Suite: "s", Name: "t"}}}
	md := markdownSummary(failing, []string{"go lines coverage fell"}, "https://example.test/run")
	for _, want := range []string{"FAILING", "| go | 90% | -% | -% |", "Regressions", "- go / s: t", "https://example.test/run"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	clean := markdownSummary(Run{Summary: summary(nil)}, nil, "")
	if !strings.Contains(clean, "passing") || strings.Contains(clean, "Regressions") || strings.Contains(clean, "History") {
		t.Errorf("clean summary:\n%s", clean)
	}
	many := Run{Summary: summary(nil)}
	for i := 0; i < 60; i++ {
		many.Failed = append(many.Failed, Failure{Name: "t"})
	}
	if got := strings.Count(markdownSummary(many, nil, ""), "\n- "); got != 50 {
		t.Errorf("listed %d failures, want 50", got)
	}
}

// statusError carries an HTTP status the way the AWS SDK's errors do.
type statusError struct{ code int }

func (e statusError) Error() string       { return fmt.Sprintf("status %d", e.code) }
func (e statusError) HTTPStatusCode() int { return e.code }

// fakeStore is an in-memory S3 that honours If-Match and If-None-Match.
type fakeStore struct {
	objects    map[string][]byte
	etags      map[string]string
	conflicts  int
	conflictIs int
	getError   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, etags: map[string]string{}, conflictIs: 412}
}

func (f *fakeStore) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.getError != nil {
		return nil, f.getError
	}
	body, ok := f.objects[*in.Key]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	etag := f.etags[*in.Key]
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body)), ETag: &etag}, nil
}

func (f *fakeStore) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	key := *in.Key
	if strings.HasSuffix(key, "history.json") && f.conflicts > 0 {
		f.conflicts--
		return nil, statusError{f.conflictIs}
	}
	_, exists := f.objects[key]
	if in.IfNoneMatch != nil && exists {
		return nil, statusError{412}
	}
	if in.IfMatch != nil && f.etags[key] != *in.IfMatch {
		return nil, statusError{412}
	}
	body, _ := io.ReadAll(in.Body)
	f.objects[key] = body
	f.etags[key] = fmt.Sprintf("%q", fmt.Sprint(len(f.etags)+1, key))
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeStore) history(t *testing.T) History {
	var h History
	if err := json.Unmarshal(f.objects[productSlug+"/history.json"], &h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPublishWritesTheRunAppendsHistoryAndRegistersDefaultBranchBuilds(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	if got, err := publish(ctx, store, "b", Run{Summary: summary(nil)}, 6); err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, ok := store.objects[productSlug+"/runs/1-1.json"]; !ok {
		t.Error("the run was not written")
	}
	var entry struct {
		Name   string
		Latest RunSummary
	}
	json.Unmarshal(store.objects["_registry/"+productSlug+".json"], &entry)
	if entry.Name != productName || entry.Latest.ID != "1-1" {
		t.Errorf("registry = %+v", entry)
	}

	feature := summary(func(s *RunSummary) { s.ID = "2-1"; s.Branch = "feature"; s.Coverage["go"] = Coverage{Lines: float(50)} })
	got, err := publish(ctx, store, "b", Run{Summary: feature}, 6)
	if err != nil || fmt.Sprint(got) != "[go lines coverage fell from 90% to 50%]" {
		t.Errorf("got %v, %v", got, err)
	}
	if len(store.history(t).Runs) != 2 {
		t.Error("the history was not appended")
	}
	json.Unmarshal(store.objects["_registry/"+productSlug+".json"], &entry)
	if entry.Latest.ID != "1-1" {
		t.Error("a branch build replaced the registry entry")
	}
}

func TestPublishRetriesAHistoryWriteThatLostARaceThenGivesUp(t *testing.T) {
	ctx := context.Background()
	racing := newFakeStore()
	racing.conflicts = 2
	if _, err := publish(ctx, racing, "b", Run{Summary: summary(nil)}, 6); err != nil || len(racing.history(t).Runs) != 1 {
		t.Errorf("err = %v", err)
	}
	stuck := newFakeStore()
	stuck.conflicts = 10
	if _, err := publish(ctx, stuck, "b", Run{Summary: summary(nil)}, 3); statusCode(err) != 412 {
		t.Errorf("err = %v", err)
	}
	broken := newFakeStore()
	broken.conflicts, broken.conflictIs = 1, 500
	if _, err := publish(ctx, broken, "b", Run{Summary: summary(nil)}, 6); statusCode(err) != 500 {
		t.Errorf("err = %v", err)
	}
}

func TestPublishSurfacesAReadErrorOtherThanAMissingHistory(t *testing.T) {
	denied := newFakeStore()
	denied.getError = statusError{403}
	if _, err := publish(context.Background(), denied, "b", Run{Summary: summary(nil)}, 6); statusCode(err) != 403 {
		t.Errorf("err = %v", err)
	}
	missing := newFakeStore()
	missing.getError = statusError{404}
	if _, err := publish(context.Background(), missing, "b", Run{Summary: summary(nil)}, 6); err != nil {
		t.Errorf("a 404 is an empty history, got %v", err)
	}
	if statusCode(errors.New("plain")) != 0 {
		t.Error("an error with no status has none")
	}
}

func TestTheCommandCollectsPublishesAndRejectsUnknownCommands(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+module+"\n\ngo 1.27\n"), 0o644)
	t.Chdir(root)
	pass := `{"Action":"pass","Package":"` + module + `","Test":"T"}`
	dir := fixtureDir(t, map[string]string{"macos-test.json": pass, "linux-test.json": pass})
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	t.Setenv("HOWAREWEDOING_BUCKET", "")
	t.Setenv("GITHUB_HEAD_REF", "")
	t.Setenv("GITHUB_REF_NAME", "main")
	noStore := func(context.Context) (objectStore, error) { return nil, errors.New("no store expected") }

	var out bytes.Buffer
	if code, err := run([]string{"collect", dir}, &out, noStore); code != 0 || err != nil {
		t.Fatalf("collect = %d, %v", code, err)
	}
	if code, err := run([]string{"publish", dir}, &out, noStore); code != 0 || err != nil || !strings.Contains(out.String(), "not publishing") {
		t.Fatalf("publish = %d, %v\n%s", code, err, out.String())
	}

	store := newFakeStore()
	t.Setenv("HOWAREWEDOING_BUCKET", "b")
	summaryFile := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summaryFile)
	out.Reset()
	if code, err := run([]string{"publish", dir}, &out, func(context.Context) (objectStore, error) { return store, nil }); code != 0 || err != nil {
		t.Fatalf("publish = %d, %v", code, err)
	}
	if md, _ := os.ReadFile(summaryFile); !strings.Contains(string(md), "History and graphs") || !strings.Contains(out.String(), "Published") {
		t.Errorf("summary:\n%s\nout:\n%s", md, out.String())
	}

	os.Remove(filepath.Join(dir, "linux-test.json"))
	run([]string{"collect", dir}, &out, noStore)
	if code, _ := run([]string{"publish", dir}, &out, func(context.Context) (objectStore, error) { return store, nil }); code != 1 {
		t.Errorf("a failing run must fail the command, got %d", code)
	}
	if code, err := run([]string{"publish", dir}, &out, noStore); code != 1 || err == nil {
		t.Errorf("a store that cannot be made is an error, got %d, %v", code, err)
	}
	if code, _ := run([]string{"nope"}, &out, noStore); code != 2 {
		t.Errorf("unknown command = %d", code)
	}
	if code, err := run([]string{"publish", t.TempDir()}, &out, noStore); code != 1 || err == nil {
		t.Error("publishing with no run.json is an error")
	}
	os.Remove(filepath.Join(root, "go.mod"))
	if code, err := run([]string{"collect", dir}, &out, noStore); code != 1 || err == nil {
		t.Error("collecting outside a module is an error")
	}
}
