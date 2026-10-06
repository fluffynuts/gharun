package main

// Recipes remember how a workflow was dispatched, minus the branch, so it can
// be run again. They live in <config>/gharun/<repo>/<name>, where <config> is
// $XDG_CONFIG_HOME or ~/.config and <repo> is the origin remote's URL
// normalised (github.com/owner/name) with "/" written as "__". Each recipe is
// one plain-text file, one option per line, meant to be edited by hand:
//
//	# gharun recipe
//	repo: github.com/owner/name
//	workflow: build.yml
//	input: environment=staging
//	input: note=two\nlines
//
// Blank lines and lines starting with # are ignored. In an input value, \n is
// a newline and \\ is a backslash.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type recipe struct {
	Name     string
	Repo     string
	Workflow string
	Inputs   map[string]string
}

// ghArgs are the arguments for `gh workflow run` (after --ref) that dispatch
// the recipe without any prompting.
func (r recipe) ghArgs() []string {
	args := []string{r.Workflow}
	for _, k := range sortedKeys(r.Inputs) {
		args = append(args, "-f", k+"="+r.Inputs[k])
	}
	return args
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// normalizeRepoURL reduces the usual spellings of a remote to host/owner/name:
// git@github.com:o/r.git, https://github.com/o/r, ssh://git@github.com/o/r.git.
func normalizeRepoURL(u string) string {
	u = strings.TrimSpace(u)
	if _, rest, ok := strings.Cut(u, "://"); ok {
		host, path, _ := strings.Cut(rest, "/")
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:] // user[:password]@
		}
		host, _, _ = strings.Cut(host, ":") // :port
		u = host + "/" + path
	} else {
		if at := strings.Index(u, "@"); at >= 0 {
			u = u[at+1:] // scp-like: git@host:owner/name
		}
		u = strings.Replace(u, ":", "/", 1)
	}
	u = strings.TrimSuffix(strings.TrimRight(u, "/"), ".git")
	return strings.ToLower(u)
}

// originKey is the normalised URL of this checkout's origin remote.
func originKey() (string, error) {
	out, err := gitOutput("remote", "get-url", "origin")
	if err != nil || out == "" {
		return "", errors.New("recipes need a git checkout with an origin remote")
	}
	return normalizeRepoURL(out), nil
}

// recipeRepoKey is the recipe key for --repo (OWNER/REPO, HOST/OWNER/REPO or
// a URL, as gh accepts), or for this checkout's origin when repo is empty.
func recipeRepoKey(repo string) (string, error) {
	switch {
	case repo == "":
		return originKey()
	case strings.Count(repo, "/") == 1 && !strings.ContainsAny(repo, ":@"):
		host := os.Getenv("GH_HOST")
		if host == "" {
			host = "github.com"
		}
		return normalizeRepoURL(host + "/" + repo), nil
	}
	return normalizeRepoURL(repo), nil
}

// findRecipe is the name in names matching want, ignoring case (an exact
// match wins if two names differ only by case).
func findRecipe(names []string, want string) (string, bool) {
	for _, n := range names {
		if n == want {
			return n, true
		}
	}
	for _, n := range names {
		if strings.EqualFold(n, want) {
			return n, true
		}
	}
	return "", false
}

func recipeDir(repoKey string) (string, error) {
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "gharun", strings.ReplaceAll(repoKey, "/", "__")), nil
}

// validRecipeName allows anything that is safe as a file name.
func validRecipeName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("a name is required")
	case name != strings.TrimSpace(name):
		return errors.New("no leading or trailing spaces")
	case strings.HasPrefix(name, "."):
		return errors.New("must not start with a dot")
	case strings.ContainsAny(name, "/\\:*?\"<>|\x00\n\r\t"):
		return errors.New(`must not contain / \ : * ? " < > | or control characters`)
	case strings.EqualFold(name, interactiveChoice):
		return errors.New(`"interactive" is the menu's own entry`)
	case len(name) > 100:
		return errors.New("too long")
	}
	return nil
}

// listRecipes names the recipes for a repository, sorted. Dotfiles and editor
// backups (name~) are not recipes.
func listRecipes(repoKey string) []string {
	dir, err := recipeDir(repoKey)
	if err != nil {
		return nil
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && !strings.HasPrefix(n, ".") && !strings.HasSuffix(n, "~") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func recipePath(repoKey, name string) (string, error) {
	dir, err := recipeDir(repoKey)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func saveRecipe(r recipe) (string, error) {
	if err := validRecipeName(r.Name); err != nil {
		return "", err
	}
	path, err := recipePath(r.Repo, r.Name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(r.format()), 0o644)
}

func loadRecipe(repoKey, name string) (recipe, error) {
	path, err := recipePath(repoKey, name)
	if err != nil {
		return recipe{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return recipe{}, err
	}
	r, err := parseRecipe(string(data))
	if err != nil {
		return recipe{}, fmt.Errorf("%s: %w", path, err)
	}
	r.Name = name
	return r, nil
}

var (
	inputEscaper   = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	inputUnescaper = strings.NewReplacer(`\\`, `\`, `\n`, "\n")
)

func (r recipe) format() string {
	var b strings.Builder
	b.WriteString("# gharun recipe: edit freely. The branch is chosen when it runs.\n")
	fmt.Fprintf(&b, "repo: %s\n", r.Repo)
	fmt.Fprintf(&b, "workflow: %s\n", r.Workflow)
	for _, k := range sortedKeys(r.Inputs) {
		fmt.Fprintf(&b, "input: %s=%s\n", k, inputEscaper.Replace(r.Inputs[k]))
	}
	return b.String()
}

func parseRecipe(text string) (recipe, error) {
	r := recipe{Inputs: map[string]string{}}
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		if !ok {
			return r, fmt.Errorf("line %d: expected \"option: value\"", n+1)
		}
		switch strings.TrimSpace(key) {
		case "repo":
			r.Repo = value
		case "workflow":
			r.Workflow = value
		case "input":
			k, v, ok := strings.Cut(value, "=")
			if !ok || k == "" {
				return r, fmt.Errorf("line %d: input needs name=value", n+1)
			}
			r.Inputs[strings.TrimSpace(k)] = inputUnescaper.Replace(v)
		default:
			return r, fmt.Errorf("line %d: unknown option %q", n+1, key)
		}
	}
	if r.Workflow == "" {
		return r, errors.New("no workflow: line")
	}
	return r, nil
}
