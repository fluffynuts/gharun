package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
