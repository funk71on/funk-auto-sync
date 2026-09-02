package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gen2brain/beeep"
)

// version is set at build time via -ldflags "-X main.version=x.y.z".
// It defaults to "dev" for local, non-release builds.
var version = "dev"

// watchRoot represents a single Git repository folder being watched.
// name is used to disambiguate log lines and notifications when several
// folders are watched at the same time.
type watchRoot struct {
	path string // absolute path to the repository root
	name string // display name (base of path)
}

// pathList implements flag.Value so -path can be passed multiple times
// (-path a -path b) and/or as a comma-separated list (-path a,b,c).
type pathList []string

func (p *pathList) String() string {
	return strings.Join(*p, ",")
}

func (p *pathList) Set(value string) error {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*p = append(*p, part)
		}
	}
	return nil
}

// resolveWatchRoots turns the raw, possibly overlapping/duplicated list of
// user-supplied paths into a deduplicated list of absolute watchRoots.
// It fails fast (returning an error) if a folder doesn't exist.
func resolveWatchRoots(rawPaths []string) ([]watchRoot, error) {
	if len(rawPaths) == 0 {
		rawPaths = []string{"."}
	}

	seen := make(map[string]bool)
	var roots []watchRoot
	for _, p := range rawPaths {
		absPath, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("invalid path %q: %v", p, err)
		}
		if seen[absPath] {
			continue // silently dedupe repeated/overlapping entries
		}
		seen[absPath] = true

		if _, err := os.Stat(absPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("folder not found: %s", absPath)
		}

		roots = append(roots, watchRoot{path: absPath, name: filepath.Base(absPath)})
	}
	return roots, nil
}

// findRootForPath returns the watchRoot that contains eventPath, choosing
// the most specific (longest) match if roots happen to be nested. This is
// how a single shared fsnotify.Watcher figures out which repository a
// changed file actually belongs to.
func findRootForPath(eventPath string, roots []watchRoot) (watchRoot, bool) {
	var best watchRoot
	found := false
	for _, r := range roots {
		if eventPath != r.path && !strings.HasPrefix(eventPath, r.path+string(filepath.Separator)) {
			continue
		}
		if !found || len(r.path) > len(best.path) {
			best = r
			found = true
		}
	}
	return best, found
}

// debouncer coalesces a burst of rapid calls for the same key into a single
// delayed call, so that e.g. an editor that writes a file several times in
// quick succession (autosave, swap-file dance, etc.) triggers one sync
// instead of several. It's a struct (not package globals) so tests can
// create an isolated instance.
type debouncer struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
}

func newDebouncer() *debouncer {
	return &debouncer{timers: make(map[string]*time.Timer)}
}

// schedule (re)starts a delay-duration timer for key, cancelling any timer
// already pending for that key. fn runs at most once per burst, after the
// key has been quiet for delay.
func (d *debouncer) schedule(key string, delay time.Duration, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[key]; ok {
		t.Stop()
	}
	d.timers[key] = time.AfterFunc(delay, func() {
		d.mu.Lock()
		delete(d.timers, key)
		d.mu.Unlock()
		fn()
	})
}

// isGitIgnored reports whether path is excluded by .gitignore (or any other
// git exclude mechanism) in the repository rooted at dir, via `git
// check-ignore`. This lets the watcher skip build output, dependency
// folders, etc. without needing its own ignore-pattern parser — it defers
// entirely to the same rules the repository already declares.
func isGitIgnored(dir, path string) bool {
	cmd := exec.Command("git", "check-ignore", "-q", path)
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return true // exit code 0: path is ignored
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false // exit code 1: path is not ignored
	}
	// Any other exit code (or failure to even run git) means check-ignore
	// couldn't give a reliable answer — fail open so a real change is never
	// silently dropped because of an unrelated git error.
	return false
}

// secretExcludePatterns lists file patterns that should never be staged by
// git add. These are appended as :(exclude) pathspecs so that even if a
// user accidentally puts a .env or private key in the watched folder, it
// won't be committed.
var secretExcludePatterns = []string{
	".env", ".env.*",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.keystore",
	"id_rsa", "id_ed25519", "id_ecdsa",
	"*credential*", "*secret*",
}

// secretContentPatterns lists substrings that, when found inside a staged
// file's content, indicate that the file likely contains secrets and the
// commit should be aborted as a safety net.
var secretContentPatterns = []string{
	"-----BEGIN PRIVATE KEY",
	"-----BEGIN RSA PRIVATE KEY",
	"-----BEGIN EC PRIVATE KEY",
	"-----BEGIN OPENSSH PRIVATE KEY",
	"AWS_SECRET_ACCESS_KEY",
	"AKIA", // AWS access key ID prefix
	"password=",
	"token=",
	"api_key=",
	"apikey=",
	"secret_key=",
}

// buildGitAddArgs constructs the arguments for `git add` with :(exclude)
// pathspecs for all secret patterns, so sensitive files are never staged.
func buildGitAddArgs() []string {
	args := []string{"add", "."}
	for _, pattern := range secretExcludePatterns {
		args = append(args, ":(exclude)"+pattern)
	}
	return args
}

// scanStagedForSecrets inspects the content of all currently staged files
// for known secret patterns. It returns a list of files that matched.
// If any are found, the caller should reset and abort the sync.
func scanStagedForSecrets(dir string) []string {
	out, err := runGitCommand(dir, "diff", "--cached", "--name-only")
	if err != nil || out == "" {
		return nil
	}

	var flagged []string
	for _, file := range strings.Split(out, "\n") {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		fullPath := filepath.Join(dir, file)
		if containsSecretContent(fullPath) {
			flagged = append(flagged, file)
		}
	}
	return flagged
}

// containsSecretContent reads a file and checks if any line contains a
// known secret pattern substring.
func containsSecretContent(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		for _, pattern := range secretContentPatterns {
			if strings.Contains(line, pattern) {
				return true
			}
		}
	}
	return false
}

// nonRetryablePatterns lists substrings in git output that indicate errors
// which will not resolve by retrying (conflicts, auth failures, etc.).
var nonRetryablePatterns = []string{
	"CONFLICT",
	"non-fast-forward",
	"authentication failed",
	"permission denied",
	"could not read Username",
	"remote: Repository not found",
	"fatal: repository",
}

// isNonRetryableError checks if the output of a failed git command contains
// a pattern indicating the error is permanent and should not be retried.
func isNonRetryableError(output string) bool {
	lower := strings.ToLower(output)
	for _, pattern := range nonRetryablePatterns {
		if strings.Contains(lower, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

// runGitCommandWithRetry wraps runGitCommand with exponential backoff
// retry logic. It retries up to maxRetries times with delays of
// 1s, 2s, 4s, etc. Non-retryable errors (conflicts, auth failures)
// cause an immediate return.
func runGitCommandWithRetry(dir string, maxRetries int, args ...string) (string, error) {
	var lastOut string
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			log.Printf("Retry %d/%d in %s for: git %s", attempt, maxRetries, delay, strings.Join(args, " "))
			time.Sleep(delay)
		}
		lastOut, lastErr = runGitCommand(dir, args...)
		if lastErr == nil {
			return lastOut, nil
		}
		if isNonRetryableError(lastOut) {
			return lastOut, lastErr
		}
	}
	return lastOut, lastErr
}

// runGitCommand executes a git command inside the target directory
func runGitCommand(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// getCurrentBranch retrieves the name of the currently active branch in the target repository
func getCurrentBranch(dir string) (string, error) {
	out, err := runGitCommand(dir, "branch", "--show-current")
	if err != nil || out == "" {
		return "", fmt.Errorf("failed to detect branch: %v", err)
	}
	return out, nil
}

// notifySuccess shows a success/info desktop notification
func notifySuccess(title, message string) {
	err := beeep.Notify(title, message, "")
	if err != nil {
		log.Printf("Failed to show success notification: %v", err)
	}
}

// notifyError shows an alert/error desktop notification
func notifyError(title, message string) {
	err := beeep.Alert(title, message, "")
	if err != nil {
		log.Printf("Failed to show error notification: %v", err)
	}
}

// checkGitInstalled verifies that the git executable is available on PATH.
// Failing fast here with a clear message is much friendlier than letting
// every subsequent git call fail with a cryptic "executable file not found".
func checkGitInstalled() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git was not found on your PATH. Please install Git and make sure the 'git' command works in your terminal")
	}
	return nil
}

// checkGitRepo verifies that dir is a Git repository with an "origin" remote
// configured, so configuration mistakes are reported once at startup instead
// of on the first file change.
func checkGitRepo(dir string) error {
	if out, err := runGitCommand(dir, "rev-parse", "--is-inside-work-tree"); err != nil || out != "true" {
		return fmt.Errorf("'%s' is not a Git repository. Run 'git init' inside it first", dir)
	}

	if out, err := runGitCommand(dir, "remote", "get-url", "origin"); err != nil || out == "" {
		return fmt.Errorf("'%s' has no 'origin' remote configured. Run 'git remote add origin <url>' first", dir)
	}

	return nil
}

// shouldSkipDir reports whether a directory should be excluded from
// recursive watching (the .git folder and any other hidden directory).
func shouldSkipDir(name string) bool {
	base := filepath.Base(name)
	return base == ".git" || (strings.HasPrefix(base, ".") && base != ".")
}

// addWatchRecursive walks the directory tree rooted at root and registers
// every subdirectory (except hidden ones and .git) with the watcher, so
// that changes in nested folders are detected as well. fsnotify only
// watches directories non-recursively, so each one has to be added
// individually.
func addWatchRecursive(watcher *fsnotify.Watcher, root string) (int, error) {
	count := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		if path != root && shouldSkipDir(path) {
			return filepath.SkipDir
		}
		if err := watcher.Add(path); err != nil {
			return fmt.Errorf("failed to watch %s: %v", path, err)
		}
		count++
		return nil
	})
	return count, err
}

// fileChange holds the per-file line-change summary and status
// (added/modified/deleted/renamed) used to build a commit message.
type fileChange struct {
	path      string
	status    string // "A" (added), "M" (modified), "D" (deleted), "R" (renamed), or "" (unknown)
	additions int
	deletions int
	isBinary  bool
}

// verbForStatus maps a git status letter to a human-readable verb.
func verbForStatus(status string) string {
	switch status {
	case "A":
		return "Add"
	case "D":
		return "Delete"
	case "R":
		return "Rename"
	default:
		return "Update"
	}
}

// buildCommitMessage inspects the currently staged changes and generates a
// descriptive commit message summarizing which files changed and how many
// lines were added/removed, instead of a generic timestamp-only message.
// If the diff cannot be parsed for any reason, it falls back to a simple
// message based on the file that triggered the sync.
func buildCommitMessage(targetDir string, fallbackFile string) string {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	fallback := fmt.Sprintf("Auto-notes update: %s (file: %s)", timestamp, filepath.Base(fallbackFile))

	numstatOut, err := runGitCommand(targetDir, "diff", "--cached", "--numstat")
	if err != nil || numstatOut == "" {
		return fallback
	}

	statusOut, err := runGitCommand(targetDir, "diff", "--cached", "--name-status")
	if err != nil {
		statusOut = ""
	}
	statusByFile := map[string]string{}
	for _, line := range strings.Split(statusOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			// Status codes can be like "R100" for renames; keep only the letter.
			statusByFile[fields[len(fields)-1]] = string(fields[0][0])
		}
	}

	var changes []fileChange
	totalAdd, totalDel := 0, 0
	for _, line := range strings.Split(numstatOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// Binary files report "-" instead of a number in additions/deletions.
		isBinary := fields[0] == "-" || fields[1] == "-"
		add := parseNumstatValue(fields[0])
		del := parseNumstatValue(fields[1])
		path := fields[2]

		changes = append(changes, fileChange{
			path:      filepath.Base(path),
			status:    statusByFile[path],
			additions: add,
			deletions: del,
			isBinary:  isBinary,
		})
		totalAdd += add
		totalDel += del
	}

	if len(changes) == 0 {
		return fallback
	}

	const maxFilesListed = 4
	var parts []string
	if len(changes) <= maxFilesListed {
		for _, c := range changes {
			if c.isBinary {
				parts = append(parts, fmt.Sprintf("%s %s (binary)", verbForStatus(c.status), c.path))
			} else {
				parts = append(parts, fmt.Sprintf("%s %s (+%d/-%d)", verbForStatus(c.status), c.path, c.additions, c.deletions))
			}
		}
	} else {
		parts = append(parts, fmt.Sprintf("Update %d files (+%d/-%d)", len(changes), totalAdd, totalDel))
	}

	summary := strings.Join(parts, "; ")
	return fmt.Sprintf("%s — %s", summary, timestamp)
}

// parseNumstatValue converts a numstat field to an int, treating the
// binary-file placeholder "-" as zero.
func parseNumstatValue(s string) int {
	if s == "-" {
		return 0
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// syncGit executes the Git pipeline: add -> commit -> pull --rebase -> push.
// label identifies which watched repo this run belongs to (its folder name);
// it's only used to make logs/notifications unambiguous when several
// folders are being watched at once, and can be left empty.
func syncGit(targetDir string, changedFile string, label string) {
	fileName := filepath.Base(changedFile)
	prefix := ""
	if label != "" {
		prefix = fmt.Sprintf("[%s] ", label)
	}
	fmt.Printf("\n[+] %sChange detected: %s\n", prefix, fileName)
	time.Sleep(500 * time.Millisecond) // Buffer to make sure the file write has finished

	// 1. Validate & get active branch
	branch, err := getCurrentBranch(targetDir)
	if err != nil {
		errMsg := fmt.Sprintf("%sFolder is not a valid Git repository, or the branch could not be found.", prefix)
		log.Printf("ERROR: %s", errMsg)
		notifyError("Auto-Notes Error", errMsg)
		return
	}
	fmt.Printf("%sActive branch: %s\n", prefix, branch)

	// 2. Git Add (with secret exclusion via pathspec)
	addArgs := buildGitAddArgs()
	fmt.Printf("%s--> Executing: git %s\n", prefix, strings.Join(addArgs, " "))
	if out, err := runGitCommand(targetDir, addArgs...); err != nil {
		errMsg := fmt.Sprintf("Git Add failed: %s", out)
		log.Printf("%s%s", prefix, errMsg)
		notifyError("Git Error", prefix+errMsg)
		return
	}

	// 2.5 Scan staged files for secret content (safety net)
	if flagged := scanStagedForSecrets(targetDir); len(flagged) > 0 {
		runGitCommand(targetDir, "reset", "HEAD")
		errMsg := fmt.Sprintf("Secret content detected in: %s — commit aborted for safety!", strings.Join(flagged, ", "))
		log.Printf("%s%s", prefix, errMsg)
		notifyError("Secret Detected!", prefix+errMsg)
		return
	}

	// 3. Git Commit — generate a descriptive message from the staged diff
	commitMsg := buildCommitMessage(targetDir, changedFile)

	fmt.Printf("%s--> Executing: git commit\n", prefix)
	fmt.Printf("    Message: %s\n", commitMsg)
	outCommit, err := runGitCommand(targetDir, "commit", "-m", commitMsg)
	if err != nil {
		if strings.Contains(outCommit, "nothing to commit") {
			fmt.Printf("%sNo changes to commit.\n", prefix)
			return
		}
		errMsg := fmt.Sprintf("Git Commit error: %s", outCommit)
		log.Printf("%s%s", prefix, errMsg)
		notifyError("Git Error", prefix+errMsg)
		return
	}

	// 4. Git Pull with Rebase (with retry for transient network errors)
	fmt.Printf("%s--> Executing: git pull --rebase (with retry)\n", prefix)
	if outPull, err := runGitCommandWithRetry(targetDir, 3, "pull", "origin", branch, "--rebase"); err != nil {
		log.Printf("%sCONFLICT DETECTED / PULL FAILED!\n%s", prefix, outPull)
		fmt.Println("Aborting rebase to keep the repository safe...")
		runGitCommand(targetDir, "rebase", "--abort")

		confMsg := fmt.Sprintf("%sGit conflict on branch %s! Manual resolution needed for file %s.", prefix, branch, fileName)
		fmt.Println(confMsg)
		notifyError("Git CONFLICT!", confMsg)
		return
	}

	// 5. Git Push (with retry for transient network errors)
	fmt.Printf("%s--> Executing: git push origin %s (with retry)\n", prefix, branch)
	if outPush, err := runGitCommandWithRetry(targetDir, 3, "push", "origin", branch); err != nil {
		errMsg := fmt.Sprintf("Git Push failed: %s", outPush)
		log.Printf("%s%s", prefix, errMsg)
		notifyError("Git Push Error", prefix+errMsg)
		return
	}

	successMsg := fmt.Sprintf("%sFile %s was successfully pushed to branch %s.", prefix, fileName, branch)
	fmt.Println("[OK] Success! " + successMsg)
	notifySuccess("Auto-Notes Synced!", successMsg)
}

func main() {
	// Flag parsing to determine the target folder path(s). -path can be
	// repeated (-path a -path b) and/or comma-separated (-path a,b).
	var rawPaths pathList
	flag.Var(&rawPaths, "path", "Path(s) of the notes directory to watch. Repeatable (-path a -path b) or comma-separated (-path a,b). Defaults to the current directory.")

	// Note on -debounce: For large Microsoft Office files (.docx, .xlsx), the saving
	// process may involve multiple write-to-temp and rename operations. If commits
	// are being triggered prematurely mid-save, test with larger files and increase
	// this value via the -debounce flag (e.g. -debounce 5s).
	debounceDelay := flag.Duration("debounce", 2*time.Second, "How long to wait for changes to settle before syncing, to coalesce rapid successive edits into one sync.")
	showVersion := flag.Bool("version", false, "Print the version number and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("funk-auto-sync %s\n", version)
		return
	}

	// Resolve, dedupe, and sanity-check every requested folder up front.
	roots, err := resolveWatchRoots(rawPaths)
	if err != nil {
		log.Fatalf("Startup check failed: %v", err)
	}

	// Pre-flight checks: fail fast with a clear message instead of erroring
	// out on the first detected file change. Every root must independently
	// be a Git repository with an "origin" remote.
	if err := checkGitInstalled(); err != nil {
		log.Fatalf("Startup check failed: %v", err)
	}
	for _, r := range roots {
		if err := checkGitRepo(r.path); err != nil {
			log.Fatalf("Startup check failed: %v", err)
		}
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	syncDebouncer := newDebouncer()

	done := make(chan bool)

	// Goroutine that watches for file events across every watched root
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				// Ignore the internal .git folder
				if strings.Contains(event.Name, ".git") {
					continue
				}

				// Handle Write or Create events
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					// If a new directory was created, start watching it too
					// so recursive watching stays up to date at runtime.
					if event.Has(fsnotify.Create) && !shouldSkipDir(event.Name) {
						if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
							if _, err := addWatchRecursive(watcher, event.Name); err != nil {
								log.Printf("Failed to watch new directory %s: %v", event.Name, err)
							} else {
								fmt.Printf("New directory detected, now watching: %s\n", event.Name)
							}
							continue
						}
					}

					// Filter out swap/temp files created by text editors and Microsoft Office lock files (e.g. ~$laporan.docx)
					baseName := filepath.Base(event.Name)
					if strings.HasSuffix(event.Name, "~") || strings.HasPrefix(baseName, ".") || strings.HasPrefix(strings.ToLower(baseName), "~$") {
						continue
					}

					// Figure out which watched repo this file actually
					// belongs to, since a single watcher now covers all of them.
					root, found := findRootForPath(event.Name, roots)
					if !found {
						log.Printf("Ignoring event for %s: no matching watched root", event.Name)
						continue
					}

					// Respect the repo's own .gitignore instead of trying
					// to reimplement its rules — build output, dependency
					// folders, etc. never need to trigger a sync.
					if isGitIgnored(root.path, event.Name) {
						continue
					}

					// Debounce per-repo: a burst of rapid edits to the same
					// (or different) files in this repo collapses into one
					// sync once things go quiet for -debounce.
					changedFile := event.Name
					syncDebouncer.schedule(root.path, *debounceDelay, func() {
						syncGit(root.path, changedFile, root.name)
					})
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("Watcher error:", err)
			}
		}
	}()

	// Recursively add every root (and its subdirectories) to the watcher
	totalWatchedDirs := 0
	var names []string
	for _, r := range roots {
		count, err := addWatchRecursive(watcher, r.path)
		if err != nil {
			log.Fatal("Failed to watch folder:", err)
		}
		totalWatchedDirs += count
		names = append(names, r.name)
	}

	fmt.Printf("Auto-Push Bot is active!\nWatching %d folder(s) (%d directories total, recursive):\n", len(roots), totalWatchedDirs)
	for _, r := range roots {
		fmt.Printf("  - %s (%s)\n", r.name, r.path)
	}
	fmt.Printf("Debounce: %s (changes are grouped if they settle within this window)\n", *debounceDelay)
	fmt.Println("Press Ctrl+C to exit.")

	// Notify once the application is active
	beeep.Notify("Auto-Notes Bot Active", "Watching "+strings.Join(names, ", "), "")

	// Block the main thread so the program keeps running
	<-done
}
