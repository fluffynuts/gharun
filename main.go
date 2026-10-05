// run-github-workflow wraps `gh workflow run`:
//  1. prompts for a remote branch (scrollable, filterable single-select)
//  2. hands the terminal to `gh workflow run --ref <branch>` so gh's own
//     workflow/input prompts work normally
//  3. finds the run that dispatch created and polls it every 5s until it
//     completes, printing the run URL and SUCCESS (green) or FAILED (red)
//
// Any extra arguments are passed through to `gh workflow run`.
// --install copies this binary to ~/.local/bin (warning if that isn't on PATH).
// --upgrade downloads the latest release for this machine and installs it.
// If --ref/-r is supplied, the branch prompt is skipped.
// Before dispatching, the checked-out branch must exist on the remote (else
// exit 1), and uncommitted or unpushed work needs confirming.
// If --repo/-R is supplied, it is applied to every gh call via GH_REPO.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

const (
	statePollInterval    = 5 * time.Second
	discoverPollInterval = 3 * time.Second
	discoverTimeout      = 90 * time.Second
	discoverSlack        = 20 * time.Second
	maxConsecutiveErrors = 5
	releaseURL           = "https://github.com/fluffynuts/gharun/releases/latest/download"
	snapshotLimit        = "50"
	discoverLimit        = "20"
)

var (
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	dim    = lipgloss.NewStyle().Faint(true)
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
)

type workflowRun struct {
	DatabaseID   int64     `json:"databaseId"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	URL          string    `json:"url"`
	WorkflowName string    `json:"workflowName"`
	CreatedAt    time.Time `json:"createdAt"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if hasFlag(args, "--install") {
		return install()
	}
	if hasFlag(args, "--upgrade") {
		return upgrade()
	}

	if _, err := exec.LookPath("gh"); err != nil {
		fail("gh CLI not found on PATH")
		return 1
	}

	repo, args, hasRepo := extractFlag(args, "-R", "--repo")
	if hasRepo {
		// gh honours GH_REPO for api placeholders, run list and run view alike,
		// so every call below targets the same repository.
		os.Setenv("GH_REPO", repo)
	}

	ref, args, hasRef := extractFlag(args, "-r", "--ref")
	// The local checkout says nothing about a different repo (-R).
	if !hasRepo {
		if code, stop := preflight(ref, hasRef); stop {
			return code
		}
	}
	if !hasRef {
		chosen, err := promptForBranch()
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return 130
			}
			fail(err.Error())
			return 1
		}
		ref = chosen
	}

	user, err := ghOutput("api", "user", "--jq", ".login")
	if err != nil {
		fail(err.Error())
		return 1
	}

	// Snapshot existing dispatch runs BEFORE dispatching. Any run that appears
	// afterwards and isn't in this set is ours. Comparing IDs rather than
	// timestamps avoids local-vs-GitHub clock skew entirely.
	before, err := listRuns(ref, user, snapshotLimit)
	if err != nil {
		fail(err.Error())
		return 1
	}
	known := make(map[int64]bool, len(before))
	for _, r := range before {
		known[r.DatabaseID] = true
	}

	fmt.Println(dim.Render(fmt.Sprintf("Dispatching against ref: %s", ref)))
	if code := dispatch(ref, args); code != 0 {
		return code
	}

	// gh prompts for the workflow and its inputs before it dispatches, which
	// can take minutes; any other run that started meanwhile is not ours. Ours
	// was created just before gh exited, so only runs created from about now
	// (by GitHub's clock) are candidates.
	cutoff := githubNow().Add(-discoverSlack)

	fmt.Println(dim.Render("Waiting for the run to appear..."))
	newRun, err := discoverRun(ref, user, known, cutoff)
	if err != nil {
		fail(err.Error())
		return 1
	}
	fmt.Printf("Watching %s: %s\n", newRun.WorkflowName, newRun.URL)
	fmt.Println(dim.Render("(Ctrl-C stops watching; the run itself keeps going)"))

	return watch(newRun.DatabaseID)
}

// hasFlag reports whether flag appears before any "--" (after which
// arguments belong to gh).
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == flag {
			return true
		}
	}
	return false
}

// install copies the running binary into ~/.local/bin and warns if that
// folder isn't on PATH.
func install() int {
	self, err := os.Executable()
	if err != nil {
		fail(err.Error())
		return 1
	}
	return installFrom(self)
}

// installFrom copies the binary at src into ~/.local/bin.
func installFrom(src string) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fail(err.Error())
		return 1
	}
	dir := filepath.Join(home, ".local", "bin")
	name := "gharun"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dest := filepath.Join(dir, name)

	if same, _ := sameFile(src, dest); same {
		fmt.Println(dim.Render(dest + " is this binary already; nothing to copy"))
	} else {
		if err := copyExecutable(src, dest); err != nil {
			fail(err.Error())
			return 1
		}
		fmt.Println(green.Render("Installed " + dest))
	}

	if !dirOnPath(dir) {
		fmt.Fprintln(os.Stderr, yellow.Render(fmt.Sprintf(
			"Warning: %s is not in your PATH; add it (e.g. export PATH=\"$HOME/.local/bin:$PATH\") to run gharun by name", dir)))
	}
	return 0
}

// upgrade downloads the latest release's zip for this OS/arch, checks it
// against the release's SHA256SUMS, and installs the binary inside it.
func upgrade() int {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	asset := fmt.Sprintf("gharun-%s-%s.zip", osName, runtime.GOARCH)
	base := os.Getenv("GHARUN_RELEASE_URL")
	if base == "" {
		base = releaseURL
	}

	fmt.Println(dim.Render("Downloading " + base + "/" + asset))
	zipData, err := httpGet(base + "/" + asset)
	if err != nil {
		fail(err.Error())
		return 1
	}
	sums, err := httpGet(base + "/SHA256SUMS")
	if err != nil {
		fail(err.Error())
		return 1
	}
	if err := verifyChecksum(asset, zipData, string(sums)); err != nil {
		fail(err.Error())
		return 1
	}

	exe := "gharun"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	dir, err := os.MkdirTemp("", "gharun-upgrade-*")
	if err != nil {
		fail(err.Error())
		return 1
	}
	defer os.RemoveAll(dir)
	extracted := filepath.Join(dir, exe)
	if err := extractBinary(zipData, exe, extracted); err != nil {
		fail(err.Error())
		return 1
	}
	return installFrom(extracted)
}

func httpGet(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// verifyChecksum looks name up in sha256sum-format sums and compares.
func verifyChecksum(name string, data []byte, sums string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	for _, line := range nonEmptyLines(sums) {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			if strings.EqualFold(fields[0], got) {
				return nil
			}
			return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", name, fields[0], got)
		}
	}
	return fmt.Errorf("%s is not listed in SHA256SUMS", name)
}

// extractBinary writes the zip entry whose base name is exe to dest. Only that
// one entry is read, and only to dest, whatever paths the zip holds.
func extractBinary(zipData []byte, exe, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || path.Base(f.Name) != exe {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
	return fmt.Errorf("no %s found in the downloaded zip", exe)
}

func sameFile(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ai, bi), nil
}

// copyExecutable writes src to a temp file beside dest and renames it into
// place, so replacing a binary that is running (or being read) is safe.
func copyExecutable(src, dest string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".gharun-install-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func dirOnPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// preflight checks the checked-out branch before anything is dispatched: the
// workflow runs whatever is pushed to the remote branch, so a branch that
// isn't there is fatal and local-only work is worth a confirmation. It does
// nothing outside a git checkout, on a detached HEAD, or when --ref names some
// other branch. stop reports whether run should return code now.
func preflight(ref string, hasRef bool) (code int, stop bool) {
	current, err := gitOutput("branch", "--show-current")
	if err != nil || current == "" || (hasRef && ref != current) {
		return 0, false
	}

	if _, err := ghOutput("api", "repos/{owner}/{repo}/branches/"+current, "--jq", ".name"); err != nil {
		if strings.Contains(err.Error(), "Not Found") || strings.Contains(err.Error(), "404") {
			fail(fmt.Sprintf("The remote branch %q doesn't exist. Push it first (git push -u origin %s).", current, current))
		} else {
			fail(err.Error())
		}
		return 1, true
	}

	var warnings []string
	if status, err := gitOutput("status", "--porcelain"); err == nil && status != "" {
		warnings = append(warnings, "You have uncommitted files")
	}
	if hasUnpushedCommits(current) {
		warnings = append(warnings, "You have unpushed changes")
	}
	if len(warnings) == 0 {
		return 0, false
	}

	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, yellow.Render(w))
	}
	proceed := false
	err = huh.NewConfirm().
		Title("Run the workflow anyway?").
		Description(fmt.Sprintf("It will run with whatever is pushed on %q on the remote.", current)).
		Affirmative("Yes, run it").
		Negative("No").
		Value(&proceed).
		Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return 130, true
		}
		fail(err.Error())
		return 1, true
	}
	if !proceed {
		return 1, true
	}
	return 0, false
}

// hasUnpushedCommits reports whether branch has commits its upstream (or,
// failing that, origin/<branch>) lacks. If neither is known locally it can't
// tell, and says no.
func hasUnpushedCommits(branch string) bool {
	for _, base := range []string{"@{upstream}", "origin/" + branch} {
		if _, err := gitOutput("rev-parse", "--verify", "--quiet", base); err != nil {
			continue
		}
		n, err := gitOutput("rev-list", "--count", base+"..HEAD")
		return err == nil && n != "0"
	}
	return false
}

// promptForBranch lists the remote's branches (the ref is resolved on GitHub,
// so local-only branches are useless here) with the current branch first.
func promptForBranch() (string, error) {
	out, err := ghOutput("api", "--paginate", "repos/{owner}/{repo}/branches", "--jq", ".[].name")
	if err != nil {
		return "", err
	}
	branches := nonEmptyLines(out)
	if len(branches) == 0 {
		return "", errors.New("no branches found on the remote")
	}
	sort.Strings(branches)

	current, _ := gitOutput("branch", "--show-current")
	ordered := make([]string, 0, len(branches))
	if current != "" && contains(branches, current) {
		ordered = append(ordered, current)
	} else if current != "" {
		fmt.Fprintln(os.Stderr, yellow.Render(
			fmt.Sprintf("Note: current branch %q is not on the remote (unpushed?)", current)))
	}
	for _, b := range branches {
		if b != current {
			ordered = append(ordered, b)
		}
	}

	selected := ordered[0]
	sel := huh.NewSelect[string]().
		Title("Branch to run against").
		Description("↑/↓ to move, / to filter, enter to select").
		Options(huh.NewOptions(ordered...)...).
		Value(&selected)
	if len(ordered) > 12 {
		sel = sel.Height(15)
	}
	if err := sel.Run(); err != nil {
		return "", err
	}
	return selected, nil
}

// dispatch runs `gh workflow run` attached to the real terminal. gh refuses
// to prompt when stdout isn't a TTY, so its output is deliberately NOT
// captured; the run is identified afterwards by ID diffing instead.
func dispatch(ref string, extra []string) int {
	ghArgs := append([]string{"workflow", "run", "--ref", ref}, extra...)
	cmd := exec.Command("gh", ghArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Ctrl-C goes to the whole foreground process group. Let gh handle it
	// and observe gh's exit status rather than dying underneath it.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fail(fmt.Sprintf("running gh workflow run: %v", err))
		return 1
	}
	return 0
}

// githubNow is the current time by GitHub's clock (the Date header of an API
// response), which avoids local clock skew; it falls back to the local clock.
func githubNow() time.Time {
	out, err := ghOutput("api", "-i", "rate_limit")
	if err == nil {
		if t, ok := dateHeader(out); ok {
			return t
		}
	}
	return time.Now()
}

func dateHeader(response string) (time.Time, bool) {
	for _, line := range strings.Split(response, "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			// the body follows the first blank line; stop looking
			if strings.TrimSpace(line) == "" {
				break
			}
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), "Date") {
			t, err := http.ParseTime(strings.TrimSpace(value))
			return t, err == nil
		}
	}
	return time.Time{}, false
}

// newRuns are the runs not in known that were created at or after cutoff,
// newest first.
func newRuns(runs []workflowRun, known map[int64]bool, cutoff time.Time) []workflowRun {
	var fresh []workflowRun
	for _, r := range runs {
		if !known[r.DatabaseID] && !r.CreatedAt.Before(cutoff) {
			fresh = append(fresh, r)
		}
	}
	sort.Slice(fresh, func(i int, j int) bool {
		return fresh[i].DatabaseID > fresh[j].DatabaseID
	})
	return fresh
}

func discoverRun(ref string, user string, known map[int64]bool, cutoff time.Time) (workflowRun, error) {
	deadline := time.Now().Add(discoverTimeout)
	for time.Now().Before(deadline) {
		runs, err := listRuns(ref, user, discoverLimit)
		if err == nil {
			fresh := newRuns(runs, known, cutoff)
			if len(fresh) > 0 {
				if len(fresh) > 1 {
					fmt.Fprintln(os.Stderr, yellow.Render(fmt.Sprintf(
						"Warning: %d new runs appeared on %s; watching the newest", len(fresh), ref)))
				}
				return fresh[0], nil
			}
		}
		time.Sleep(discoverPollInterval)
	}
	return workflowRun{}, fmt.Errorf(
		"no new workflow_dispatch run appeared on %q within %s", ref, discoverTimeout)
}

func watch(id int64) int {
	idStr := fmt.Sprintf("%d", id)
	lastStatus := ""
	errorCount := 0
	started := time.Now()

	for {
		out, err := ghOutput("run", "view", idStr, "--json", "status,conclusion,url,workflowName")
		if err != nil {
			errorCount++
			fmt.Fprintln(os.Stderr, yellow.Render(fmt.Sprintf(
				"poll error (%d/%d): %v", errorCount, maxConsecutiveErrors, err)))
			if errorCount >= maxConsecutiveErrors {
				fail("giving up after repeated poll errors")
				return 1
			}
			time.Sleep(statePollInterval)
			continue
		}
		errorCount = 0

		var r workflowRun
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			fail(fmt.Sprintf("parsing run state: %v", err))
			return 1
		}

		if r.Status != lastStatus {
			elapsed := time.Since(started).Round(time.Second)
			fmt.Println(dim.Render(fmt.Sprintf("[%s] status: %s", elapsed, r.Status)))
			lastStatus = r.Status
		}

		if r.Status == "completed" {
			if r.Conclusion == "success" {
				fmt.Println(green.Render(r.URL))
				fmt.Println(green.Render("SUCCESS"))
				return 0
			}
			label := "FAILED"
			if r.Conclusion != "failure" {
				label = fmt.Sprintf("FAILED (%s)", r.Conclusion)
			}
			fmt.Println(red.Render(r.URL))
			fmt.Println(red.Render(label))
			return 1
		}

		time.Sleep(statePollInterval)
	}
}

func listRuns(ref string, user string, limit string) ([]workflowRun, error) {
	out, err := ghOutput("run", "list",
		"--branch", ref,
		"--event", "workflow_dispatch",
		"--user", user,
		"--limit", limit,
		"--json", "databaseId,status,conclusion,url,workflowName,createdAt")
	if err != nil {
		return nil, err
	}
	var runs []workflowRun
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		return nil, fmt.Errorf("parsing run list: %w", err)
	}
	return runs, nil
}

// extractFlag pulls a string flag out of args in any pflag form:
// "-r x", "-rx", "-r=x", "--ref x", "--ref=x". Parsing stops at "--".
func extractFlag(args []string, short string, long string) (string, []string, bool) {
	rest := make([]string, 0, len(args))
	value := ""
	found := false

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		switch {
		case a == short || a == long:
			if i+1 < len(args) {
				value = args[i+1]
				found = true
				i++
				continue
			}
			rest = append(rest, a)
		case strings.HasPrefix(a, long+"="):
			value = strings.TrimPrefix(a, long+"=")
			found = true
		case strings.HasPrefix(a, short+"="):
			value = strings.TrimPrefix(a, short+"=")
			found = true
		case strings.HasPrefix(a, short) && !strings.HasPrefix(a, "--") && len(a) > len(short):
			value = strings.TrimPrefix(a, short)
			found = true
		default:
			rest = append(rest, a)
		}
	}
	return value, rest, found
}

func ghOutput(args ...string) (string, error) {
	return commandOutput("gh", args...)
}

func gitOutput(args ...string) (string, error) {
	return commandOutput("git", args...)
}

func commandOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func nonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			lines = append(lines, t)
		}
	}
	return lines
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, red.Render("error: "+msg))
}
