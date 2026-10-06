package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

	serve := func(sums string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/" + asset:
				rw.Write(buf.Bytes())
			case "/SHA256SUMS":
				fmt.Fprint(rw, sums)
			default:
				http.NotFound(rw, r)
			}
		}))
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", "") // no gharun on PATH: upgrade installs to ~/.local/bin

	bad := serve(fmt.Sprintf("%064x  %s\n", 0, asset))
	defer bad.Close()
	t.Setenv("GHARUN_RELEASE_URL", bad.URL)
	if code := upgrade(); code == 0 {
		t.Fatal("upgrade accepted a checksum mismatch")
	}

	good := serve(fmt.Sprintf("%x  %s\n", sum, asset))
	defer good.Close()
	t.Setenv("GHARUN_RELEASE_URL", good.URL)
	if code := upgrade(); code != 0 {
		t.Fatalf("upgrade exited %d", code)
	}
	got, err := os.ReadFile(filepath.Join(home, ".local", "bin", exe))
	if err != nil || string(got) != "new binary" {
		t.Fatalf("installed binary = %q, %v", got, err)
	}
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
		{[]string{"--recipe"}, "", ""},
		{[]string{"--recipe", "-r", "main"}, "", "main"},
		{[]string{"--recipe", "deploy to staging", "-r", "main"}, "deploy to staging", "main"},
		{[]string{"--recipe=Deploy"}, "Deploy", ""},
	} {
		o, err := parseArgs(c.args)
		if err != nil || !o.recipe || o.recipeName != c.name || o.ref != c.ref {
			t.Errorf("parseArgs(%q) = %+v, %v", c.args, o, err)
		}
	}
	for _, bad := range [][]string{{"--recipe="}, {"--recipe", "x", "--capture"}, {"--recipe", "x", "--", "a.yml"}} {
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
	stdout, _, code = captureRun(t, "-R", "o/r", "--recipe")
	if code != 0 || stdout != "deploy\nnightly\n" {
		t.Fatalf("listing: code %d, stdout %q", code, stdout)
	}
}

// captureRun calls run with args and returns what it printed and its exit code.
func captureRun(t *testing.T, args ...string) (stdout, stderr string, code int) {
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
	code = run(args)
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
