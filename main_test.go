package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

func TestUpgradeInstallsLatestRelease(t *testing.T) {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	exe := "gharun"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	asset := fmt.Sprintf("gharun-%s-%s.zip", osName, runtime.GOARCH)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("gharun-0.1.9-" + osName + "-" + runtime.GOARCH + "/" + exe)
	w.Write([]byte("new binary"))
	zw.Close()
	sum := sha256.Sum256(buf.Bytes())

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", "") // no gharun on PATH: upgrade installs to ~/.local/bin
	setVersion(t, "0.1", "8")

	t.Setenv("GHARUN_RELEASE_URL", serveReleases(t, "v0.1.9", map[string][]byte{
		asset:        buf.Bytes(),
		"SHA256SUMS": []byte(fmt.Sprintf("%064x  %s\n", 0, asset)),
	}))
	if code := upgrade(); code == 0 {
		t.Fatal("upgrade accepted a checksum mismatch")
	}

	t.Setenv("GHARUN_RELEASE_URL", serveReleases(t, "v0.1.9", map[string][]byte{
		asset:        buf.Bytes(),
		"SHA256SUMS": []byte(fmt.Sprintf("%x  %s\n", sum, asset)),
	}))
	if code := upgrade(); code != 0 {
		t.Fatalf("upgrade exited %d", code)
	}
	got, err := os.ReadFile(filepath.Join(home, ".local", "bin", exe))
	if err != nil || string(got) != "new binary" {
		t.Fatalf("installed binary = %q, %v", got, err)
	}
}

func TestUpgradeDoesNothingWhenAlreadyOnTheLatestRelease(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", "")
	setVersion(t, "0.1", "9")
	// No assets: any download fails the upgrade.
	t.Setenv("GHARUN_RELEASE_URL", serveReleases(t, "v0.1.9", nil))

	if code := upgrade(); code != 0 {
		t.Fatalf("upgrade exited %d", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("upgrade installed something when already up to date (stat: %v)", err)
	}
}

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		latest, running string
		want            bool
	}{
		{"0.1.9", "0.1.8", true},
		{"0.1.9", "0.1.9", false},
		{"0.1.9", "0.1.10", false},
		{"0.2.1", "0.1.99", true},
		{"0.1.9", "0.1", true}, // a local build of 0.1 predates every 0.1 release
		{"0.1.9", "", true},    // a dev build with no version at all
	}
	for _, c := range cases {
		if got := newerVersion(c.latest, c.running); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.latest, c.running, got, c.want)
		}
	}
}

// setVersion stands in for the version make.sh bakes in, for the rest of t.
func setVersion(t *testing.T, version, build string) {
	t.Helper()
	oldVersion, oldBuild := Version, Build
	t.Cleanup(func() { Version, Build = oldVersion, oldBuild })
	Version, Build = version, build
}

// serveReleases stands in for GitHub's releases page, whose latest release
// is tag with assets (name → content): /releases/latest redirects to the
// tag, and each asset is at /releases/download/<tag>/<name>. It returns the
// releases page's URL.
func serveReleases(t *testing.T, tag string, assets map[string][]byte) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/"+tag+"/", func(rw http.ResponseWriter, r *http.Request) {
		if content, ok := assets[path.Base(r.URL.Path)]; ok {
			rw.Write(content)
			return
		}
		http.NotFound(rw, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/releases"
}

func TestUpgradeTargetIsTheGharunOnPath(t *testing.T) {
	exe := "gharun"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	t.Setenv("PATH", t.TempDir())
	got, err := upgradeTarget()
	if want := filepath.Join(home, ".local", "bin", exe); err != nil || got != want {
		t.Fatalf("with no gharun on PATH, upgradeTarget = %q, %v; want %q", got, err, want)
	}

	real := filepath.Join(t.TempDir(), exe)
	if err := os.WriteFile(real, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	pathed := t.TempDir()
	t.Setenv("PATH", pathed)
	if runtime.GOOS == "windows" {
		pathed = filepath.Dir(real) // symlinks need privileges on Windows
		t.Setenv("PATH", pathed)
	} else if err := os.Symlink(real, filepath.Join(pathed, exe)); err != nil {
		t.Fatal(err)
	}
	got, err = upgradeTarget()
	wantReal, _ := filepath.EvalSymlinks(real)
	if err != nil || got != wantReal {
		t.Fatalf("upgradeTarget = %q, %v; want %q (the symlink's target)", got, err, wantReal)
	}
}

func TestNewRunsIgnoresRunsThatStartedBeforeTheDispatch(t *testing.T) {
	cutoff := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	runs := []workflowRun{
		{DatabaseID: 9, CreatedAt: cutoff.Add(-5 * time.Minute)}, // started while gh was prompting
		{DatabaseID: 5, CreatedAt: cutoff.Add(-time.Hour)},       // known
		{DatabaseID: 7, CreatedAt: cutoff.Add(time.Second)},      // ours
	}
	got := newRuns(runs, map[int64]bool{5: true}, cutoff)
	if len(got) != 1 || got[0].DatabaseID != 7 {
		t.Fatalf("newRuns = %+v, want only run 7", got)
	}
}

func TestDateHeader(t *testing.T) {
	resp := "HTTP/2.0 200 OK\nContent-Type: application/json\nDate: Mon, 05 Oct 2026 12:00:01 GMT\n\n{\"a\": \"Date: nope\"}"
	got, ok := dateHeader(resp)
	if !ok || !got.Equal(time.Date(2026, 10, 5, 12, 0, 1, 0, time.UTC)) {
		t.Fatalf("dateHeader = %v, %v", got, ok)
	}
}

func TestFormatClock(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                     "00:00",
		31*time.Second + 900*time.Millisecond: "00:31",
		754 * time.Second:                     "12:34",
		3661 * time.Second:                    "1:01:01",
	} {
		if got := formatClock(d); got != want {
			t.Errorf("formatClock(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	o, err := parseArgs([]string{"-r", "staging", "-Rfoo/bar", "--version", "--", "deploy.yml", "-r", "x", "--bogus"})
	if err != nil || o.ref != "staging" || o.repo != "foo/bar" || !o.version {
		t.Fatalf("parseArgs = %+v, %v", o, err)
	}
	if got := fmt.Sprint(o.passthrough); got != "[deploy.yml -r x --bogus]" {
		t.Fatalf("passthrough = %s", got)
	}
	for _, bad := range [][]string{{"deploy.yml"}, {"--bogus"}, {"-r"}, {"--ref="}, {"-f", "a=b"}} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("parseArgs(%v) accepted bad arguments", bad)
		}
	}
}

func TestParseRecipeArgs(t *testing.T) {
	for _, c := range []struct {
		args []string
		name string
		ref  string
	}{
		{[]string{"--recipe", "deploy to staging", "-r", "main"}, "deploy to staging", "main"},
		{[]string{"--recipe=Deploy"}, "Deploy", ""},
	} {
		o, err := parseArgs(c.args)
		if err != nil || !o.recipe || o.recipeName != c.name || o.ref != c.ref {
			t.Errorf("parseArgs(%q) = %+v, %v", c.args, o, err)
		}
	}
	for _, bad := range [][]string{{"--recipe="}, {"--recipe"}, {"--recipe", "-r", "main"}, {"--recipe", "x", "--", "a.yml"}} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("parseArgs(%q) accepted bad arguments", bad)
		}
	}
}

func TestFindRecipe(t *testing.T) {
	names := []string{"Deploy", "deploy", "nightly build"}
	for want, got := range map[string]string{"DEPLOY": "Deploy", "deploy": "deploy", "Nightly Build": "nightly build"} {
		if n, ok := findRecipe(names, want); !ok || n != got {
			t.Errorf("findRecipe(%q) = %q, %v; want %q", want, n, ok, got)
		}
	}
	if _, ok := findRecipe(names, "nightly"); ok {
		t.Error("findRecipe matched part of a name")
	}
}

func TestRecipeRepoKey(t *testing.T) {
	t.Setenv("GH_HOST", "")
	for repo, want := range map[string]string{
		"Fluffynuts/gharun":                    "github.com/fluffynuts/gharun",
		"github.example/acme/app":              "github.example/acme/app",
		"https://github.com/fluffynuts/gharun": "github.com/fluffynuts/gharun",
	} {
		if got, err := recipeRepoKey(repo); err != nil || got != want {
			t.Errorf("recipeRepoKey(%q) = %q, %v; want %q", repo, got, err, want)
		}
	}
	t.Setenv("GH_HOST", "github.example")
	if got, _ := recipeRepoKey("acme/app"); got != "github.example/acme/app" {
		t.Errorf("with GH_HOST, recipeRepoKey = %q", got)
	}
}

func TestRecipeNotFoundListsRecipes(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("GH_HOST", "")
	for _, n := range []string{"deploy", "nightly"} {
		if _, err := saveRecipe(recipe{Name: n, Repo: "github.com/o/r", Workflow: "a.yml"}); err != nil {
			t.Fatal(err)
		}
	}
	stdout, stderr, code := captureRun(t, "-R", "o/r", "--recipe", "bogus")
	if code != 1 || stdout != "" ||
		!strings.Contains(stderr, "recipe not found: bogus") ||
		!strings.Contains(stderr, "Available recipes:\n  deploy\n  nightly\n") {
		t.Fatalf("code %d\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	stdout, stderr, code = captureRun(t, "-R", "o/r", "--recipe")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "needs the name of a recipe") || !strings.Contains(stderr, "--recipes") {
		t.Fatalf("bare --recipe: code %d\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
}

// captureRun calls run with args and returns what it printed and its exit code.
func captureRun(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return captureOutput(t, func() int { return run(args) })
}

// captureOutput calls f and returns what it printed and the code f returned.
func captureOutput(t *testing.T, f func() int) (stdout, stderr string, code int) {
	t.Helper()
	read := func(f **os.File) func() string {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		orig := *f
		*f = w
		done := make(chan string)
		go func() {
			b, _ := io.ReadAll(r)
			done <- string(b)
		}()
		return func() string {
			w.Close()
			*f = orig
			return <-done
		}
	}
	outDone, errDone := read(&os.Stdout), read(&os.Stderr)
	code = f()
	return outDone(), errDone(), code
}

func TestFindDispatchAndCommand(t *testing.T) {
	log := "* Request to https://api.github.com/x\n> GET /repos/o/r/actions/workflows HTTP/1.1\n\n{}\n\n" +
		"* Request at 2026-10-05 13:45:32 +0000 UTC\n" +
		"* Request to https://github.example/api/v3/repos/acme/app/actions/workflows/1234/dispatches\n" +
		"> POST /api/v3/repos/acme/app/actions/workflows/1234/dispatches HTTP/1.1\n" +
		"> Host: github.example\n> Authorization: token ████████████████████\n> Content-Length: 38\n\n" +
		"{\n  \"inputs\": {\n    \"env\": \"prod\",\n    \"note\": \"it's fine\"\n  },\n  \"ref\": \"staging\"\n}\n\n" +
		"< HTTP/1.1 204 No Content\n"
	d, ok := findDispatch(log)
	if !ok {
		t.Fatal("dispatch not found")
	}
	d.Workflow = "build.yml"
	want := `gh workflow run build.yml -R acme/app --ref staging -f env=prod -f 'note=it'\''s fine'`
	if got := d.command(); got != want {
		t.Fatalf("command = %s\nwant      %s", got, want)
	}
	if _, ok := findDispatch("> GET /repos/o/r HTTP/1.1\n"); ok {
		t.Fatal("found a dispatch where there is none")
	}
}

func TestFindDispatchToleratesUnprefixedHeadersAndTypedInputs(t *testing.T) {
	log := "POST /repos/o/r/actions/workflows/9/dispatches HTTP/1.1\nHost: api.github.com\n\n" +
		`{"inputs":{"debug":true,"n":3,"s":"x"},"ref":"main"}` + "\n\n"
	// no "> " prefix on the request line: not recognised, and says so
	if _, ok := findDispatch(log); ok {
		t.Fatal("unexpectedly matched an unprefixed request line")
	}
	d, ok := findDispatch("> POST /repos/o/r/actions/workflows/9/dispatches HTTP/1.1\nHost: api.github.com\n\n" +
		`{"inputs":{"debug":true,"n":3,"s":"x"},"ref":"main"}` + "\n\n")
	if !ok || d.Inputs["debug"] != "true" || d.Inputs["n"] != "3" || d.Inputs["s"] != "x" {
		t.Fatalf("findDispatch = %+v, %v", d, ok)
	}
}

func TestNormalizeRepoURL(t *testing.T) {
	for _, u := range []string{
		"git@github.com:Fluffynuts/gharun.git",
		"https://github.com/fluffynuts/gharun",
		"https://user:pw@github.com/fluffynuts/gharun.git/",
		"ssh://git@github.com/fluffynuts/gharun.git",
		"ssh://git@github.com:22/fluffynuts/gharun",
	} {
		if got := normalizeRepoURL(u); got != "github.com/fluffynuts/gharun" {
			t.Errorf("normalizeRepoURL(%q) = %q", u, got)
		}
	}
}

func TestRecipeRoundTrip(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	r := recipe{Name: "deploy to staging", Repo: "github.com/o/r", Workflow: "build.yml",
		Inputs: map[string]string{"env": "staging", "note": "two\nlines \\ ok", "empty": ""}}
	path, err := saveRecipe(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(config, "gharun", "github.com__o__r", "deploy to staging"); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	got, err := loadRecipe(r.Repo, r.Name)
	if err != nil || fmt.Sprint(got) != fmt.Sprint(r) {
		t.Fatalf("loaded %+v, %v; want %+v", got, err, r)
	}
	if names := listRecipes("github.com/o/r"); fmt.Sprint(names) != "[deploy to staging]" {
		t.Fatalf("listRecipes = %v", names)
	}
	if listRecipes("github.com/o/other") != nil {
		t.Fatal("recipes leaked between repositories")
	}
	if args := fmt.Sprint(r.ghArgs()); args != "[build.yml -f empty= -f env=staging -f note=two\nlines \\ ok]" {
		t.Fatalf("ghArgs = %q", args)
	}
	for _, bad := range []string{"", " x", "../x", "a/b", ".hidden", "a:b"} {
		if validRecipeName(bad) == nil {
			t.Errorf("validRecipeName(%q) accepted", bad)
		}
	}
	if _, err := parseRecipe("workflow: a.yml\nbogus: 1\n"); err == nil {
		t.Error("parseRecipe accepted an unknown option")
	}
}

func TestParseRunURL(t *testing.T) {
	for _, u := range []string{
		"https://github.com/o/r/actions/runs/123",
		"https://github.com/o/r/actions/runs/123/job/456?pr=7",
		"https://github.com/o/r/actions/runs/123/attempts/2",
	} {
		host, repo, id, err := parseRunURL(u)
		if err != nil || host != "github.com" || repo != "o/r" || id != 123 {
			t.Errorf("parseRunURL(%q) = %s, %s, %d, %v", u, host, repo, id, err)
		}
	}
	for _, u := range []string{"123", "https://github.com/o/r", "https://github.com/o/r/actions/workflows/build.yml", "https://github.com/o/r/actions/runs/abc"} {
		if _, _, _, err := parseRunURL(u); err == nil {
			t.Errorf("parseRunURL(%q) accepted", u)
		}
	}
	if o, err := parseArgs([]string{"--watch=https://x/o/r/actions/runs/1"}); err != nil || o.watchURL == "" {
		t.Errorf("--watch= not parsed: %+v, %v", o, err)
	}
	if _, err := parseArgs([]string{"--watch"}); err == nil {
		t.Error("--watch without a URL accepted")
	}
}

func TestClockElapsed(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	now := t0.Add(30 * time.Second)
	// not started: our own clock
	if got := clockElapsed(t0, now, 0, time.Time{}, time.Time{}); got != 30*time.Second {
		t.Errorf("before start = %v", got)
	}
	// started on GitHub's clock 5 minutes ago, our clock 2 seconds behind it
	runStart := now.Add(2*time.Second - 5*time.Minute)
	if got := clockElapsed(t0, now, 2*time.Second, runStart, time.Time{}); got != 5*time.Minute {
		t.Errorf("running = %v", got)
	}
	// finished: the run's own duration, whatever the time now
	if got := clockElapsed(t0, now.Add(time.Hour), 0, runStart, runStart.Add(90*time.Second)); got != 90*time.Second {
		t.Errorf("finished = %v", got)
	}
}

func TestRunProgress(t *testing.T) {
	job := func(name, status string) workflowJob { return workflowJob{Name: name, Status: status} }
	cases := []struct {
		name        string
		run         workflowRun
		width       int
		want        string
		wantStarted bool
	}{
		{
			name: "nothing started yet",
			run:  workflowRun{Status: "queued", Jobs: []workflowJob{job("test", "queued")}},
			want: "queued",
		},
		{
			name: "no jobs yet",
			run:  workflowRun{Status: "queued"},
			want: "queued",
		},
		{
			name:        "a job running",
			run:         workflowRun{Status: "in_progress", Jobs: []workflowJob{job("check", "completed"), job("build (24.x)", "in_progress"), job("test (24.x)", "in_progress"), job("deploy", "queued")}},
			want:        "running: 1/4 jobs done; build (24.x) in progress; test (24.x) in progress; deploy waiting for a runner",
			wantStarted: true,
		},
		{
			// GitHub calls the run queued while a later job waits for a runner,
			// however much of it has already run.
			name:        "queued run with jobs done",
			run:         workflowRun{Status: "queued", Jobs: []workflowJob{job("check-master-merge", "completed"), job("test (24.x)", "queued"), job("build-and-push (24.x)", "completed"), job("deploy-without-tests", "completed")}},
			want:        "running: 3/4 jobs done; test (24.x) waiting for a runner",
			wantStarted: true,
		},
		{
			name:        "waiting on an environment's approval",
			run:         workflowRun{Status: "waiting", Jobs: []workflowJob{job("build", "completed"), job("deploy", "waiting")}},
			want:        "running: 1/2 jobs done; deploy waiting for approval",
			wantStarted: true,
		},
		{
			name:        "too narrow for every job",
			run:         workflowRun{Status: "in_progress", Jobs: []workflowJob{job("aaaa", "in_progress"), job("bbbb", "in_progress"), job("cccc", "in_progress")}},
			width:       len("running: 0/3 jobs done; aaaa in progress; +2 more") + 3,
			want:        "running: 0/3 jobs done; aaaa in progress; +2 more",
			wantStarted: true,
		},
		{
			name:        "too narrow for any job",
			run:         workflowRun{Status: "in_progress", Jobs: []workflowJob{job("a-very-long-job-name", "in_progress")}},
			width:       len("running: 0/1 jobs done; +1 more"),
			want:        "running: 0/1 jobs done; +1 more",
			wantStarted: true,
		},
	}
	for _, c := range cases {
		got, started := runProgress(c.run, c.width)
		if got != c.want || started != c.wantStarted {
			t.Errorf("%s: runProgress = %q, %v; want %q, %v", c.name, got, started, c.want, c.wantStarted)
		}
	}
}

func TestCommandOutputWithinGivesUpOnAHungCommand(t *testing.T) {
	if os.Getenv("GHARUN_TEST_HANG") == "1" {
		time.Sleep(time.Minute)
		return
	}
	t.Setenv("GHARUN_TEST_HANG", "1")
	started := time.Now()
	_, err := commandOutputWithin(200*time.Millisecond, os.Args[0], "-test.run=^TestCommandOutputWithinGivesUpOnAHungCommand$")
	if err == nil || !strings.Contains(err.Error(), "timed out after 200ms") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("took %v to give up", took)
	}
	if got := pollErrorReason(err); got != "timed out after 200ms" {
		t.Errorf("pollErrorReason = %q, want just the timeout", got)
	}
}

func TestPollErrorReasonIsWhatTheCommandSaid(t *testing.T) {
	if os.Getenv("GHARUN_TEST_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "HTTP 502: Bad Gateway (https://api.github.com/...)")
		fmt.Fprintln(os.Stderr, "try again later")
		os.Exit(1)
	}
	t.Setenv("GHARUN_TEST_FAIL", "1")
	_, err := commandOutputWithin(time.Minute, os.Args[0], "-test.run=^TestPollErrorReasonIsWhatTheCommandSaid$")
	if err == nil {
		t.Fatal("a failing command succeeded")
	}
	if got := pollErrorReason(err); got != "HTTP 502: Bad Gateway (https://api.github.com/...)" {
		t.Errorf("pollErrorReason = %q, want the first line of its stderr", got)
	}
	if got := pollErrorReason(errors.New("parsing run state: bad json\nmore")); got != "parsing run state: bad json" {
		t.Errorf("pollErrorReason of another error = %q, want its first line", got)
	}
}

func TestStatusLinePipedPrintsChangesOnly(t *testing.T) {
	var out bytes.Buffer
	s := statusLine{w: &out}
	s.show("[00:01] ", "queued", "", 0)
	s.show("[00:02] ", "queued", "", 0) // only the clock moved
	s.show("[00:35] ", "queued", " (poll error 1/5: timed out after 30s)", 0)
	s.show("[01:10] ", "queued", " (poll error 2/5: timed out after 30s)", 0)
	s.show("[01:15] ", "queued", "", 0)
	want := "[00:01] queued\n" +
		"[00:35] queued (poll error 1/5: timed out after 30s)\n" +
		"[01:10] queued (poll error 2/5: timed out after 30s)\n" +
		"[01:15] queued\n"
	if got := out.String(); got != want {
		t.Errorf("printed:\n%s\nwant:\n%s", got, want)
	}
}

func TestStatusLineLiveStaysWithinTheTerminal(t *testing.T) {
	var out bytes.Buffer
	s := statusLine{w: &out, live: true}
	s.show("[00:35] ", "running: 3/4 jobs done; +1 more", "", 30)
	got := out.String()
	if !strings.HasPrefix(got, "\r\x1b[2K") {
		t.Fatalf("live line %q doesn't redraw in place", got)
	}
	if w := lipgloss.Width(strings.TrimPrefix(got, "\r\x1b[2K")); w >= 30 {
		t.Errorf("live line %q is %d wide; a 30-column terminal would wrap it", got, w)
	}
}

func TestStatusLineLiveKeepsAPollErrorInView(t *testing.T) {
	var out bytes.Buffer
	s := statusLine{w: &out, live: true}
	s.show("[00:35] ", "running: 3/4 jobs done; +1 more", " (poll error 1/5: timed out after 30s)", 60)
	got := strings.TrimPrefix(out.String(), "\r\x1b[2K")
	if w := lipgloss.Width(got); w >= 60 {
		t.Errorf("live line %q is %d wide; a 60-column terminal would wrap it", got, w)
	}
	if !strings.HasSuffix(got, " (poll error 1/5: timed out after 30s)") {
		t.Errorf("live line %q cut off the poll error rather than the status", got)
	}
}

func TestParseSaveLastArgs(t *testing.T) {
	if o, err := parseArgs([]string{"--save-last"}); err != nil || !o.saveLast {
		t.Errorf("parseArgs(--save-last) = %+v, %v", o, err)
	}
	for _, bad := range [][]string{
		{"--save-last", "--recipe", "x"},
		{"--save-last", "-r", "main"},
		{"--save-last", "-R", "o/r"},
		{"--save-last", "--watch", "https://github.com/o/r/actions/runs/1"},
		{"--save-last", "--", "a.yml"},
		{"-r", "main", "--save-last", "--", "a.yml"},
	} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("parseArgs(%q) accepted bad arguments", bad)
		}
	}
	if _, err := parseArgs([]string{"--capture"}); err == nil {
		t.Error("--capture was removed but is still accepted")
	}
}

func TestLastRunRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	key := "github.com/o/r"
	if _, found, err := loadLastRun(key); found || err != nil {
		t.Fatalf("fresh state: found %v, err %v", found, err)
	}
	last := recipe{Repo: key, Workflow: "build.yml", Inputs: map[string]string{"env": "staging", "note": "a\nb"}}
	if err := rememberLastRun(last); err != nil {
		t.Fatal(err)
	}
	got, found, err := loadLastRun(key)
	if err != nil || !found || got.Workflow != "build.yml" || got.Inputs["env"] != "staging" || got.Inputs["note"] != "a\nb" {
		t.Fatalf("loadLastRun = %+v, %v, %v", got, found, err)
	}
	if _, found, _ := loadLastRun("github.com/o/other"); found {
		t.Error("last run leaked between repositories")
	}
	if names := listRecipes(key); len(names) != 0 {
		t.Errorf("last run showed up as a recipe: %v", names)
	}
	if err := forgetLastRun(key); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := loadLastRun(key); found {
		t.Error("last run still there after forgetLastRun")
	}
	if err := forgetLastRun(key); err != nil {
		t.Errorf("forgetting nothing failed: %v", err)
	}
}

func TestLastRunIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix permissions")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := rememberLastRun(recipe{Repo: "github.com/o/r", Workflow: "a.yml"}); err != nil {
		t.Fatal(err)
	}
	path, _ := lastRunPath("github.com/o/r")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode: %v, %v", info, err)
	}
}

func TestRecordLastRunSkipsWhatIsAlreadyARecipe(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	key := "github.com/o/r"
	inputs := map[string]string{"env": "staging"}
	if _, err := saveRecipe(recipe{Name: "staging", Repo: key, Workflow: "a.yml", Inputs: inputs}); err != nil {
		t.Fatal(err)
	}
	// an older, different last run must not linger once a newer run is already a recipe
	if err := rememberLastRun(recipe{Repo: key, Workflow: "old.yml"}); err != nil {
		t.Fatal(err)
	}

	recordLastRun(recipe{Repo: key, Workflow: "a.yml", Inputs: map[string]string{"env": "staging"}})
	if _, found, _ := loadLastRun(key); found {
		t.Error("recorded a run identical to an existing recipe")
	}

	recordLastRun(recipe{Repo: key, Workflow: "a.yml", Inputs: map[string]string{"env": "prod"}})
	got, found, _ := loadLastRun(key)
	if !found || got.Inputs["env"] != "prod" {
		t.Errorf("did not record a new run: %+v, %v", got, found)
	}
}

func TestSaveLastTurnsLastRunIntoRecipe(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	key := "github.com/o/r"
	name := func(string) (string, error) { return "staging", nil }

	_, stderr, code := captureOutput(t, func() int { return saveLastAsRecipe(key, name) })
	if code != 1 || !strings.Contains(stderr, "nothing to save") {
		t.Fatalf("with no last run: code %d, stderr %q", code, stderr)
	}

	if err := rememberLastRun(recipe{Repo: key, Workflow: "build.yml", Inputs: map[string]string{"env": "staging"}}); err != nil {
		t.Fatal(err)
	}
	stdout, _, code := captureOutput(t, func() int { return saveLastAsRecipe(key, name) })
	if code != 0 || !strings.Contains(stdout, "build.yml") || !strings.Contains(stdout, "env=staging") {
		t.Fatalf("saving: code %d, stdout %q", code, stdout)
	}
	r, err := loadRecipe(key, "staging")
	if err != nil || r.Workflow != "build.yml" || r.Inputs["env"] != "staging" {
		t.Fatalf("saved recipe = %+v, %v", r, err)
	}

	_, stderr, code = captureOutput(t, func() int { return saveLastAsRecipe(key, name) })
	if code != 1 || !strings.Contains(stderr, "nothing to save") {
		t.Fatalf("second save: code %d, stderr %q", code, stderr)
	}
}

func TestSaveLastKeepsLastRunWhenAborted(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	key := "github.com/o/r"
	if err := rememberLastRun(recipe{Repo: key, Workflow: "build.yml"}); err != nil {
		t.Fatal(err)
	}
	abort := func(string) (string, error) { return "", huh.ErrUserAborted }
	if _, _, code := captureOutput(t, func() int { return saveLastAsRecipe(key, abort) }); code != 130 {
		t.Fatalf("aborting gave code %d, want 130", code)
	}
	if _, found, _ := loadLastRun(key); !found {
		t.Error("last run was purged although no recipe was saved")
	}
}

func TestRecipesListsRecipesForTheRepository(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GH_HOST", "")

	stdout, stderr, code := captureRun(t, "-R", "o/r", "--recipes")
	if code != 0 || stdout != "" || !strings.Contains(stderr, "No recipes saved yet") {
		t.Fatalf("no recipes: code %d\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}

	for _, n := range []string{"nightly", "deploy"} {
		if _, err := saveRecipe(recipe{Name: n, Repo: "github.com/o/r", Workflow: "a.yml"}); err != nil {
			t.Fatal(err)
		}
	}
	stdout, stderr, code = captureRun(t, "-R", "o/r", "--recipes")
	if code != 0 || stdout != "deploy\nnightly\n" || stderr != "" {
		t.Fatalf("listing: code %d\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
}

func TestParseRecipesArgs(t *testing.T) {
	if o, err := parseArgs([]string{"--recipes", "-R", "o/r"}); err != nil || !o.recipes || o.repo != "o/r" {
		t.Errorf("parseArgs(--recipes -R o/r) = %+v, %v", o, err)
	}
	for _, bad := range [][]string{
		{"--recipes", "--recipe", "x"},
		{"--recipes", "--save-last"},
		{"--recipes", "-r", "main"},
		{"--recipes", "--", "a.yml"},
	} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("parseArgs(%q) accepted bad arguments", bad)
		}
	}
}

func TestManageRecipesDeletesOnlyWhatWasSelectedAndConfirmed(t *testing.T) {
	setup := func() string {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		key := "github.com/o/r"
		for _, n := range []string{"a", "b", "c"} {
			if _, err := saveRecipe(recipe{Name: n, Repo: key, Workflow: "w.yml"}); err != nil {
				t.Fatal(err)
			}
		}
		return key
	}
	choose := func(names ...string) func([]string) ([]string, error) {
		return func(all []string) ([]string, error) {
			if fmt.Sprint(all) != "[a b c]" {
				t.Errorf("offered %v", all)
			}
			return names, nil
		}
	}
	confirm := func(answer bool, asked *[]string) func([]string) (bool, error) {
		return func(names []string) (bool, error) {
			*asked = names
			return answer, nil
		}
	}
	remaining := func(key string) string { return fmt.Sprint(listRecipes(key)) }

	key := setup()
	var asked []string
	if _, _, code := captureOutput(t, func() int { return manageRecipes(key, choose("a", "c"), confirm(true, &asked)) }); code != 0 {
		t.Fatalf("code %d", code)
	}
	if fmt.Sprint(asked) != "[a c]" || remaining(key) != "[b]" {
		t.Errorf("confirmed deletion: asked %v, left %s", asked, remaining(key))
	}

	key = setup()
	asked = nil
	captureOutput(t, func() int { return manageRecipes(key, choose("a"), confirm(false, &asked)) })
	if remaining(key) != "[a b c]" {
		t.Errorf("deleted although declined: left %s", remaining(key))
	}

	key = setup()
	asked = nil
	captureOutput(t, func() int { return manageRecipes(key, choose(), confirm(true, &asked)) })
	if asked != nil || remaining(key) != "[a b c]" {
		t.Errorf("nothing selected: asked %v, left %s", asked, remaining(key))
	}

	key = setup()
	abortChoose := func([]string) ([]string, error) { return nil, huh.ErrUserAborted }
	if _, _, code := captureOutput(t, func() int { return manageRecipes(key, abortChoose, confirm(true, &asked)) }); code != 130 || remaining(key) != "[a b c]" {
		t.Errorf("abort while choosing: code %d, left %s", code, remaining(key))
	}
	abortConfirm := func([]string) (bool, error) { return false, huh.ErrUserAborted }
	if _, _, code := captureOutput(t, func() int { return manageRecipes(key, choose("a"), abortConfirm) }); code != 130 || remaining(key) != "[a b c]" {
		t.Errorf("abort while confirming: code %d, left %s", code, remaining(key))
	}
}

func TestIsTerminalIsFalseForDevNullPipesAndFiles(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip("no " + os.DevNull)
	}
	defer devNull.Close()
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	file, _ := os.Create(filepath.Join(t.TempDir(), "f"))
	defer file.Close()
	for name, f := range map[string]*os.File{"/dev/null": devNull, "pipe": r, "regular file": file} {
		if isTerminal(f) {
			t.Errorf("isTerminal(%s) = true", name)
		}
	}
}

func TestSaveLastSavesUnderTheRepositoryItWasAskedFor(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	key := "github.com/o/r"
	path, _ := lastRunPath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("workflow: build.yml\n"), 0o600); err != nil { // no repo: line
		t.Fatal(err)
	}
	captureOutput(t, func() int { return saveLastAsRecipe(key, func(string) (string, error) { return "staging", nil }) })
	if _, err := loadRecipe(key, "staging"); err != nil {
		t.Fatalf("recipe not saved for %s: %v (found: %v)", key, err, listRecipes(""))
	}
}

func TestRememberDispatch(t *testing.T) {
	key := "github.com/o/r"
	origin := func() (string, error) { return key, nil }
	noOrigin := func() (string, error) { return "", errors.New("no origin") }
	d := dispatchRequest{Workflow: "build.yml", Inputs: map[string]string{"env": "staging"}}
	fresh := func() {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	}
	last := func() (recipe, bool) {
		r, found, _ := loadLastRun(key)
		return r, found
	}

	fresh()
	captureOutput(t, func() int { rememberDispatch(false, origin, true, d); return 0 })
	if r, found := last(); !found || r.Workflow != "build.yml" || r.Inputs["env"] != "staging" {
		t.Errorf("understood dispatch: remembered %+v, %v", r, found)
	}

	fresh()
	asked := false
	captureOutput(t, func() int {
		rememberDispatch(true, func() (string, error) { asked = true; return key, nil }, true, d)
		return 0
	})
	if _, found := last(); found || asked {
		t.Errorf("--repo run: remembered %v, looked up origin %v", found, asked)
	}

	fresh()
	if err := rememberLastRun(recipe{Repo: key, Workflow: "older.yml"}); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, _ := captureOutput(t, func() int { rememberDispatch(false, origin, false, d); return 0 })
	if _, found := last(); found {
		t.Error("an older run was left as the last run after one that couldn't be read")
	}
	if !strings.Contains(stderr, "can't be saved as a recipe") || stdout != "" {
		t.Errorf("no warning that the run can't be saved: stdout %q, stderr %q", stdout, stderr)
	}

	fresh()
	_, stderr, _ = captureOutput(t, func() int { rememberDispatch(false, noOrigin, true, d); return 0 })
	if _, found := last(); found || !strings.Contains(stderr, "no origin") {
		t.Errorf("origin failure: remembered %v, stderr %q", found, stderr)
	}
}
