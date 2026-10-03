// SPDX-License-Identifier: GPL-3.0-or-later

// Command quality turns one CI build's test, coverage and govulncheck output
// into the JSON that https://howarewedoing.maragato.ca shows, and publishes it.
//
//	go run ./cmd/quality collect <dir>   -> <dir>/run.json
//	go run ./cmd/quality publish <dir>   -> S3 (needs AWS credentials)
//
// <dir> holds what the build produced, by fixed name (see areas):
// `go test -json` output and a -coverprofile per platform the tests ran on,
// and `govulncheck -json` in govulncheck.json. An area whose test report is
// missing counts as failed: a test step that crashed before writing one must
// not look like a clean run.
//
// The site is public. Only numbers, test names, package paths and the first
// line of a failure go into the JSON - never source, stack traces, or the
// details of a vulnerability.
//
// The same tool exists in JavaScript for the other products on the site; the
// JSON shapes here have to stay identical to theirs, because one page renders
// them all.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	productSlug    = "mymicrotunnel"
	productName    = "MyMicroTunnel"
	defaultBranch  = "main"
	coverageTarget = 98
	// A drop bigger than this against the last default-branch build fails the build.
	regressionTolerance = 0.5
	historyLimit        = 1000
	vulnReport          = "govulncheck.json"
)

type areaSpec struct{ name, tests, cover string }

// The macOS and Linux jobs compile different _darwin and _linux files, so each
// is an area of its own, and overall coverage is the union of the two.
var areas = []areaSpec{
	{"macos", "macos-test.json", "macos.cover"},
	{"linux", "linux-test.json", "linux.cover"},
}

type Coverage struct {
	Lines      *float64 `json:"lines"`
	Branches   *float64 `json:"branches"`
	Functions  *float64 `json:"functions"`
	Statements *float64 `json:"statements"`
}

type Security struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Moderate int `json:"moderate"`
	Low      int `json:"low"`
}

type Tests struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

type Suite struct {
	Area string `json:"area"`
	Name string `json:"name"`
	Tests
	DurationMs int64 `json:"durationMs"`
}

type Failure struct {
	Area    string `json:"area"`
	Suite   string `json:"suite"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

type FileCoverage struct {
	Area      string   `json:"area"`
	File      string   `json:"file"`
	Lines     *float64 `json:"lines"`
	Branches  *float64 `json:"branches"`
	Functions *float64 `json:"functions"`
}

type Context struct {
	ID        string  `json:"id"`
	Time      string  `json:"time"`
	Branch    string  `json:"branch"`
	Commit    string  `json:"commit"`
	CommitURL *string `json:"commitUrl"`
	BuildURL  *string `json:"buildUrl"`
	Workflow  string  `json:"workflow"`
}

type RunSummary struct {
	Context
	Status   string              `json:"status"`
	Tests    Tests               `json:"tests"`
	Coverage map[string]Coverage `json:"coverage"`
	Overall  Coverage            `json:"overall"`
	Security Security            `json:"security"`
}

type Run struct {
	Summary RunSummary     `json:"summary"`
	Suites  []Suite        `json:"suites"`
	Failed  []Failure      `json:"failed"`
	Files   []FileCoverage `json:"files"`
}

type Product struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type History struct {
	Product       Product      `json:"product"`
	DefaultBranch string       `json:"defaultBranch"`
	Runs          []RunSummary `json:"runs"`
}

func pct(hit, found int) *float64 {
	if found <= 0 {
		return nil
	}
	v := math.Round(1000*float64(hit)/float64(found)) / 10
	return &v
}

// relativePath turns an import path inside the module into a repository path,
// "." for the module root.
func relativePath(importPath, module string) string {
	if importPath == module {
		return "."
	}
	return strings.TrimPrefix(importPath, module+"/")
}

var absoluteSource = regexp.MustCompile(`(?:/[\w.@-]+)+/([\w.@-]+\.go)`)

// firstLine keeps one line with no directories: enough to know what broke
// without leaking the runner's filesystem or a stack trace onto a public page.
func firstLine(message string) string {
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = absoluteSource.ReplaceAllString(line, "$1")
		if len(line) > 300 {
			line = line[:300]
		}
		return line
	}
	return ""
}

type testEvent struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
	Output  string
}

type testResult struct {
	action  string
	elapsed float64
	output  []string
}

// parseGoTest reads `go test -json`. Only leaf tests are counted: a parent
// whose subtests ran is a grouping, and counting it too would make every
// table-driven test look like one more than it is. A package that failed
// with no failing test failed to build or crashed, and counts as a failure.
func parseGoTest(r io.Reader, area, module string) ([]Suite, []Failure, error) {
	results := map[string]map[string]*testResult{}
	packageFailed := map[string]bool{}
	packageOutput := map[string][]string{}
	var order []string

	dec := json.NewDecoder(r)
	for {
		var e testEvent
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, fmt.Errorf("reading go test output: %w", err)
		}
		if _, seen := results[e.Package]; !seen {
			results[e.Package] = map[string]*testResult{}
			order = append(order, e.Package)
		}
		if e.Test == "" {
			if e.Action == "fail" {
				packageFailed[e.Package] = true
			}
			if e.Action == "output" {
				packageOutput[e.Package] = append(packageOutput[e.Package], e.Output)
			}
			continue
		}
		t := results[e.Package][e.Test]
		if t == nil {
			t = &testResult{}
			results[e.Package][e.Test] = t
		}
		switch e.Action {
		case "pass", "fail", "skip":
			t.action, t.elapsed = e.Action, e.Elapsed
		case "output":
			t.output = append(t.output, e.Output)
		}
	}

	var suites []Suite
	var failed []Failure
	for _, pkg := range order {
		name := relativePath(pkg, module)
		suite := Suite{Area: area, Name: name}
		var names []string
		for test := range results[pkg] {
			names = append(names, test)
		}
		sort.Strings(names)
		for _, test := range names {
			t := results[pkg][test]
			if t.action == "" || hasSubtests(test, names) {
				continue
			}
			suite.Total++
			suite.DurationMs += int64(math.Round(t.elapsed * 1000))
			switch t.action {
			case "pass":
				suite.Passed++
			case "skip":
				suite.Skipped++
			case "fail":
				suite.Failed++
				failed = append(failed, Failure{Area: area, Suite: name, Name: test, Message: failureMessage(t.output)})
			}
		}
		if packageFailed[pkg] && suite.Failed == 0 {
			suite.Total++
			suite.Failed++
			failed = append(failed, Failure{Area: area, Suite: name, Name: "(package failed to build or run)", Message: failureMessage(packageOutput[pkg])})
		}
		if suite.Total > 0 {
			suites = append(suites, suite)
		}
	}
	return suites, failed, nil
}

func hasSubtests(test string, names []string) bool {
	for _, other := range names {
		if strings.HasPrefix(other, test+"/") {
			return true
		}
	}
	return false
}

// failureMessage skips the runner's own bookkeeping lines and keeps the first
// thing the test said.
func failureMessage(output []string) string {
	for _, line := range output {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "=== ") || strings.HasPrefix(trimmed, "--- ") ||
			trimmed == "FAIL" || strings.HasPrefix(trimmed, "FAIL\t") || strings.HasPrefix(trimmed, "PASS") {
			continue
		}
		return firstLine(trimmed)
	}
	return ""
}

type statementTotals struct{ found, hit int }

// parseCoverProfile reads a -coverprofile. Go measures statements, not lines,
// branches or functions, so statements stand in for lines and the other two
// are left as no data rather than a made-up number.
func parseCoverProfile(text, area, module string) ([]FileCoverage, statementTotals) {
	type block struct{ statements, count int }
	blocks := map[string]map[string]block{}
	var order []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		colon := strings.LastIndex(fields[0], ":")
		if colon < 0 {
			continue
		}
		file, where := fields[0][:colon], fields[0][colon+1:]
		statements, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil {
			continue
		}
		if blocks[file] == nil {
			blocks[file] = map[string]block{}
			order = append(order, file)
		}
		// Packages tested together can report the same block twice.
		if prior, ok := blocks[file][where]; !ok || count > prior.count {
			blocks[file][where] = block{statements, count}
		}
	}

	var files []FileCoverage
	var totals statementTotals
	for _, file := range order {
		var f statementTotals
		for _, b := range blocks[file] {
			f.found += b.statements
			if b.count > 0 {
				f.hit += b.statements
			}
		}
		totals.found += f.found
		totals.hit += f.hit
		files = append(files, FileCoverage{Area: area, File: relativePath(file, module), Lines: pct(f.hit, f.found)})
	}
	return files, totals
}

func coverageFromTotals(t statementTotals) Coverage {
	return Coverage{Lines: pct(t.hit, t.found), Statements: pct(t.hit, t.found)}
}

// vulnCounts reads `govulncheck -json`. govulncheck reports no severity, and
// only a vulnerability the code actually calls makes it fail, so each called
// vulnerability counts once as high: it is what the build has to fix.
func vulnCounts(r io.Reader) (Security, error) {
	called := map[string]bool{}
	dec := json.NewDecoder(r)
	for {
		var message struct {
			Finding *struct {
				OSV   string `json:"osv"`
				Trace []struct {
					Function string `json:"function"`
				} `json:"trace"`
			} `json:"finding"`
		}
		if err := dec.Decode(&message); err == io.EOF {
			break
		} else if err != nil {
			return Security{}, err
		}
		if f := message.Finding; f != nil && len(f.Trace) > 0 && f.Trace[0].Function != "" {
			called[f.OSV] = true
		}
	}
	return Security{High: len(called)}, nil
}

func buildContext(env func(string) string, now time.Time) Context {
	or := func(values ...string) string {
		for _, v := range values {
			if v != "" {
				return v
			}
		}
		return ""
	}
	server := or(env("GITHUB_SERVER_URL"), "https://github.com")
	repo := env("GITHUB_REPOSITORY")
	sha := or(env("QUALITY_COMMIT"), env("GITHUB_SHA"))
	runID := or(env("GITHUB_RUN_ID"), strconv.FormatInt(now.UnixMilli(), 10))
	c := Context{
		ID:       regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(runID+"-"+or(env("GITHUB_RUN_ATTEMPT"), "1"), ""),
		Time:     now.UTC().Format("2006-01-02T15:04:05.000Z"),
		Branch:   or(env("GITHUB_HEAD_REF"), env("GITHUB_REF_NAME"), "local"),
		Commit:   sha,
		Workflow: or(env("QUALITY_WORKFLOW"), env("GITHUB_WORKFLOW"), "local"),
	}
	if repo != "" && sha != "" {
		c.CommitURL = aws.String(fmt.Sprintf("%s/%s/commit/%s", server, repo, sha))
	}
	if repo != "" && env("GITHUB_RUN_ID") != "" {
		c.BuildURL = aws.String(fmt.Sprintf("%s/%s/actions/runs/%s", server, repo, env("GITHUB_RUN_ID")))
	}
	return c
}

func readIf(dir, name string) ([]byte, bool) {
	body, err := os.ReadFile(filepath.Join(dir, name))
	return body, err == nil
}

func collect(dir, module string, context Context) (Run, error) {
	run := Run{Suites: []Suite{}, Failed: []Failure{}, Files: []FileCoverage{}}
	coverage := map[string]Coverage{}
	var security Security
	overall := Coverage{}

	var profiles []string
	for _, a := range areas {
		if body, ok := readIf(dir, a.tests); ok {
			suites, failed, err := parseGoTest(bytes.NewReader(body), a.name, module)
			if err != nil {
				return Run{}, err
			}
			run.Suites = append(run.Suites, suites...)
			run.Failed = append(run.Failed, failed...)
		} else {
			run.Suites = append(run.Suites, Suite{Area: a.name, Name: "(no test report)", Tests: Tests{Total: 1, Failed: 1}})
			run.Failed = append(run.Failed, Failure{Area: a.name, Suite: "(no test report)", Name: a.name + " tests did not produce a report", Message: "The test step crashed or was skipped."})
		}
		if body, ok := readIf(dir, a.cover); ok {
			files, totals := parseCoverProfile(string(body), a.name, module)
			run.Files = append(run.Files, files...)
			coverage[a.name] = coverageFromTotals(totals)
			profiles = append(profiles, string(body))
		}
	}
	if len(profiles) > 0 {
		// A block both platforms compile is counted once, covered if either ran it.
		_, totals := parseCoverProfile(strings.Join(profiles, "\n"), "", module)
		overall = coverageFromTotals(totals)
	}
	if body, ok := readIf(dir, vulnReport); ok {
		// govulncheck prints non-JSON when it cannot reach the database; the
		// build's own log says so, and a missing count is not a zero.
		if counts, err := vulnCounts(bytes.NewReader(body)); err == nil {
			security = counts
		}
	}

	var tests Tests
	for _, s := range run.Suites {
		tests.Total += s.Total
		tests.Passed += s.Passed
		tests.Failed += s.Failed
		tests.Skipped += s.Skipped
	}
	status := "passed"
	if tests.Failed > 0 {
		status = "failed"
	}
	run.Summary = RunSummary{Context: context, Status: status, Tests: tests, Coverage: coverage, Overall: overall, Security: security}
	return run, nil
}

// findRegressions compares with the latest passing default-branch build of
// the same workflow.
func findRegressions(summary RunSummary, history *History) []string {
	if history == nil {
		return nil
	}
	var baseline *RunSummary
	for i := len(history.Runs) - 1; i >= 0; i-- {
		r := history.Runs[i]
		if r.Branch == defaultBranch && r.Status == "passed" && r.Workflow == summary.Workflow {
			baseline = &history.Runs[i]
			break
		}
	}
	if baseline == nil {
		return nil
	}
	var problems []string
	areas := make([]string, 0, len(summary.Coverage))
	for a := range summary.Coverage {
		areas = append(areas, a)
	}
	sort.Strings(areas)
	for _, a := range areas {
		before, ok := baseline.Coverage[a]
		if !ok {
			continue
		}
		now := summary.Coverage[a]
		for _, m := range []struct {
			name      string
			now, then *float64
		}{{"lines", now.Lines, before.Lines}, {"branches", now.Branches, before.Branches}, {"functions", now.Functions, before.Functions}} {
			if m.now != nil && m.then != nil && *m.now < *m.then-regressionTolerance {
				problems = append(problems, fmt.Sprintf("%s %s coverage fell from %s%% to %s%%", a, m.name, number(m.then), number(m.now)))
			}
		}
	}
	if summary.Tests.Total < baseline.Tests.Total {
		problems = append(problems, fmt.Sprintf("test count fell from %d to %d", baseline.Tests.Total, summary.Tests.Total))
	}
	return problems
}

func appendRun(history *History, summary RunSummary) History {
	runs := []RunSummary{}
	if history != nil {
		for _, r := range history.Runs {
			if r.ID != summary.ID {
				runs = append(runs, r)
			}
		}
	}
	runs = append(runs, summary)
	if len(runs) > historyLimit {
		runs = runs[len(runs)-historyLimit:]
	}
	return History{Product: Product{Slug: productSlug, Name: productName}, DefaultBranch: defaultBranch, Runs: runs}
}

func number(v *float64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func markdownSummary(run Run, regressions []string, siteURL string) string {
	s := run.Summary
	state := "passing"
	if s.Status != "passed" {
		state = "FAILING"
	}
	lines := []string{
		"### Quality: " + state,
		fmt.Sprintf("Tests: %d total, %d passed, **%d failed**, %d skipped", s.Tests.Total, s.Tests.Passed, s.Tests.Failed, s.Tests.Skipped),
		"",
		"| Area | Lines | Branches | Functions |",
		"|---|---|---|---|",
	}
	areas := make([]string, 0, len(s.Coverage))
	for a := range s.Coverage {
		areas = append(areas, a)
	}
	sort.Strings(areas)
	for _, a := range areas {
		c := s.Coverage[a]
		lines = append(lines, fmt.Sprintf("| %s | %s%% | %s%% | %s%% |", a, number(c.Lines), number(c.Branches), number(c.Functions)))
	}
	lines = append(lines,
		fmt.Sprintf("| **overall** | %s%% | %s%% | %s%% |", number(s.Overall.Lines), number(s.Overall.Branches), number(s.Overall.Functions)),
		"",
		fmt.Sprintf("Coverage target: %d%%. Security advisories: %d critical, %d high, %d moderate, %d low.", coverageTarget, s.Security.Critical, s.Security.High, s.Security.Moderate, s.Security.Low),
	)
	if len(regressions) > 0 {
		lines = append(lines, "", "**Regressions:**")
		for _, r := range regressions {
			lines = append(lines, "- "+r)
		}
	}
	if len(run.Failed) > 0 {
		lines = append(lines, "", "**Failed tests:**")
		for i, f := range run.Failed {
			if i == 50 {
				break
			}
			lines = append(lines, fmt.Sprintf("- %s / %s: %s", f.Area, f.Suite, f.Name))
		}
	}
	if siteURL != "" {
		lines = append(lines, "", "History and graphs: "+siteURL)
	}
	return strings.Join(lines, "\n")
}

// --- S3 publishing -------------------------------------------------------

type objectStore interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

func statusCode(err error) int {
	var withStatus interface{ HTTPStatusCode() int }
	if errors.As(err, &withStatus) {
		return withStatus.HTTPStatusCode()
	}
	return 0
}

func readHistory(ctx context.Context, store objectStore, bucket, key string) (*History, *string, error) {
	res, err := store.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key})
	var missing *types.NoSuchKey
	if errors.As(err, &missing) || statusCode(err) == 404 {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	var history History
	if err := json.NewDecoder(res.Body).Decode(&history); err != nil {
		return nil, nil, err
	}
	return &history, res.ETag, nil
}

func putJSON(ctx context.Context, store objectStore, bucket, key string, body any, condition func(*s3.PutObjectInput)) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	input := &s3.PutObjectInput{
		Bucket: &bucket, Key: &key, Body: bytes.NewReader(encoded),
		ContentType: aws.String("application/json"), CacheControl: aws.String("max-age=60"),
	}
	if condition != nil {
		condition(input)
	}
	_, err = store.PutObject(ctx, input)
	return err
}

// publish writes the run, then the history. history.json is shared by
// concurrent builds, so it is written with an S3 conditional put and retried
// when another build got there first.
func publish(ctx context.Context, store objectStore, bucket string, run Run, attempts int) ([]string, error) {
	if err := putJSON(ctx, store, bucket, productSlug+"/runs/"+run.Summary.ID+".json", run, nil); err != nil {
		return nil, err
	}
	key := productSlug + "/history.json"
	var regressions []string
	for attempt := 1; ; attempt++ {
		history, etag, err := readHistory(ctx, store, bucket, key)
		if err != nil {
			return nil, err
		}
		regressions = findRegressions(run.Summary, history)
		err = putJSON(ctx, store, bucket, key, appendRun(history, run.Summary), func(in *s3.PutObjectInput) {
			if etag != nil {
				in.IfMatch = etag
			} else {
				in.IfNoneMatch = aws.String("*")
			}
		})
		if err == nil {
			break
		}
		if code := statusCode(err); (code == 412 || code == 409) && attempt < attempts {
			continue
		}
		return nil, err
	}
	if run.Summary.Branch == defaultBranch {
		entry := map[string]any{"name": productName, "latest": run.Summary}
		if err := putJSON(ctx, store, bucket, "_registry/"+productSlug+".json", entry, nil); err != nil {
			return nil, err
		}
	}
	return regressions, nil
}

// --- CLI -----------------------------------------------------------------

func modulePath() (string, error) {
	body, err := os.ReadFile("go.mod")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", errors.New("go.mod names no module")
}

func run(args []string, stdout io.Writer, newStore func(context.Context) (objectStore, error)) (int, error) {
	if len(args) < 1 || (args[0] != "collect" && args[0] != "publish") {
		fmt.Fprintln(os.Stderr, "usage: quality collect|publish <dir>")
		return 2, nil
	}
	dir := "quality"
	if len(args) > 1 {
		dir = args[1]
	}
	siteURL := os.Getenv("HOWAREWEDOING_URL")
	if siteURL == "" {
		siteURL = "https://howarewedoing.maragato.ca"
	}

	if args[0] == "collect" {
		module, err := modulePath()
		if err != nil {
			return 1, err
		}
		r, err := collect(dir, module, buildContext(os.Getenv, time.Now()))
		if err != nil {
			return 1, err
		}
		encoded, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "run.json"), encoded, 0o644); err != nil {
			return 1, err
		}
		fmt.Fprintf(stdout, "%s: %d of %d tests failed, lines %s%%\n", r.Summary.Status, r.Summary.Tests.Failed, r.Summary.Tests.Total, number(r.Summary.Overall.Lines))
		return 0, nil
	}

	body, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return 1, err
	}
	var r Run
	if err := json.Unmarshal(body, &r); err != nil {
		return 1, err
	}
	bucket := os.Getenv("HOWAREWEDOING_BUCKET")
	var regressions []string
	link := ""
	if bucket != "" {
		ctx := context.Background()
		store, err := newStore(ctx)
		if err != nil {
			return 1, err
		}
		if regressions, err = publish(ctx, store, bucket, r, 6); err != nil {
			return 1, err
		}
		fmt.Fprintf(stdout, "Published %s to %s/product.html?p=%s\n", r.Summary.ID, siteURL, productSlug)
		link = fmt.Sprintf("%s/run.html?p=%s&id=%s", siteURL, productSlug, r.Summary.ID)
	} else {
		fmt.Fprintln(stdout, "HOWAREWEDOING_BUCKET is not set; not publishing.")
	}
	md := markdownSummary(r, regressions, link)
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return 1, err
		}
		defer f.Close()
		fmt.Fprintln(f, md)
	} else {
		fmt.Fprintln(stdout, md)
	}
	for _, problem := range regressions {
		fmt.Fprintln(stdout, "::error::Regression: "+problem)
	}
	if r.Summary.Status == "passed" && len(regressions) == 0 {
		return 0, nil
	}
	return 1, nil
}

func main() {
	code, err := run(os.Args[1:], os.Stdout, func(ctx context.Context) (objectStore, error) {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, err
		}
		return s3.NewFromConfig(cfg), nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
