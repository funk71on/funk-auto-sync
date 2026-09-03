
<p align="center">
  <img src="https://repository-images.githubusercontent.com/1354322046/ddfbf34c-1967-40f3-9218-6cbd6fea7905" alt="FUNK-AUTO-SYNC" width="100%">
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat-square&logo=go" alt="Go version">
  <img src="https://img.shields.io/badge/license-MIT-f59e0b?style=flat-square" alt="MIT License">
  <img src="https://img.shields.io/badge/retry-exp.%20backoff%20(3x)-8b5cf6?style=flat-square" alt="Exponential Backoff">
  <br>
  <img src="https://img.shields.io/badge/multi--folder-live%20recursive-0284c7?style=flat-square" alt="Multi-folder live recursive">
  <img src="https://img.shields.io/badge/secret%20shield-pathspec%20%2B%20scan-10b981?style=flat-square" alt="Secret shield">
  <img src="https://img.shields.io/badge/office%20%26%20docs-docx%20%E2%80%A2%20xlsx%20%E2%80%A2%20binary-6366f1?style=flat-square" alt="Office and docs">
</p>

# funk-auto-sync

A lightweight, zero-config CLI tool that automatically syncs one or more Git repositories on every local file change. Written in Go, single static binary, no background daemon required.

Designed for note-taking vaults (Obsidian, Logseq, Foam, plain Markdown), office documents (`.docx`, `.xlsx`), dotfiles, and personal config folders where you want continuous, hands-off backup to a remote Git repository without having to remember `git add`, `commit`, and `push`.

```
 edit file → detected → gitignore & lock-file check → debounce → add (exclude secrets) → scan staged → commit → pull --rebase (retry) → push (retry) → notified
```

## Table of contents

- [Features](#features)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Installation](#installation)
- [Usage](#usage)
- [How it works](#how-it-works)
- [Watching multiple folders](#watching-multiple-folders)
- [Office & Binary file support](#office--binary-file-support)
- [Handling conflicts](#handling-conflicts)
- [Running as a background service](#running-as-a-background-service-optional)
- [Reducing binary size](#reducing-binary-size)
- [Development & Tests](#development--tests)
- [Project structure](#project-structure)
- [Notes & limitations](#notes--limitations)
- [Design choices & how this compares](#design-choices--how-this-compares)
- [License](#license)

## Features

| Feature | Description |
|---|---|
| **Multi-folder watching** | Watch any number of independent Git repositories in a single process via `-path a -path b` or `-path a,b`. |
| **Recursive watching** | Automatically discovers and watches all subdirectories on startup and registers newly created ones at runtime. |
| **Debounce per repo** | Rapid successive edits (e.g. editor autosaves, swap files, multi-step office saves) collapse into a single commit once the cooldown window passes (`-debounce`, default `2s`). |
| **Descriptive commit messages** | Generates human-readable commit messages from the staged diff (e.g. `Update notes.md (+12/-3); Add todo.md (+5) — 2026-08-31 10:15:00`). |
| **Binary file friendly** | Handles binary files like `.docx`, `.xlsx`, `.pdf`, and images cleanly with descriptive `(binary)` commit summaries instead of misleading `(+0/-0)`. |
| **Office lock-file filtering** | Automatically ignores temporary lock files created by Word and Excel (`~$*.docx`, `~$*.xlsx`) to prevent sync noise and race conditions. |
| **Gitignore aware** | Uses `git check-ignore` so repository-level and global ignore rules are respected without reimplementing them. |
| **Secret protection** | Appends `:(exclude)` pathspecs for sensitive patterns (`.env*`, `*.pem`, `*.key`, credentials) on `git add`, plus secondary staged content scanning for private keys and tokens. |
| **Push/Pull retry** | Automatically retries transient network failures on `git pull --rebase` and `git push` with exponential backoff (1s, 2s, 4s). |
| **Safe rebase on conflict** | If `git pull --rebase` runs into a merge conflict, it automatically runs `git rebase --abort` to keep the local repo clean, preserving your commit, and sends an alert. |
| **Desktop notifications** | Cross-platform notifications on success, conflicts, and errors via native system notifications. |
| **Pre-flight sanity checks** | Fails fast on startup if `git` is missing, or if any configured folder is not a valid Git repo with an `origin` remote. |
| **Zero external runtime dependencies** | Single static Go binary that shells out to the user's existing `git` installation. |

## Requirements

- [Go](https://go.dev/) 1.21 or newer (to build from source)
- [Git](https://git-scm.com/) installed and available on your `PATH`
- A configured Git repository with an `origin` remote for every folder you want to watch

## Quick start

```bash
# Clone the repository
git clone https://github.com/funk71on/funk-auto-sync.git
cd funk-auto-sync

# Build the binary
go build -o funk-auto-sync main.go

# Start watching your notes folder
./funk-auto-sync -path ~/my-notes
```

## Installation

### From source

```bash
git clone https://github.com/funk71on/funk-auto-sync.git
cd funk-auto-sync
make build
```

The compiled binary will be placed at `./funk-auto-sync` (or `funk-auto-sync.exe` on Windows).

To install it directly into your `$GOPATH/bin`:

```bash
go install .
```

### Pre-built binaries

Pre-built static binaries for Linux, macOS, and Windows are available on the [Releases](https://github.com/funk71on/funk-auto-sync/releases) page.

## Usage

```bash
# Watch the current directory
./funk-auto-sync

# Watch a specific directory
./funk-auto-sync -path /path/to/notes

# Windows (PowerShell)
.\funk-auto-sync.exe -path "D:\NOTES-CLOUD"

# Watch multiple directories at once (repeatable or comma-separated)
./funk-auto-sync -path /path/to/notes -path /path/to/journal
./funk-auto-sync -path "/path/to/notes,/path/to/journal"

# Adjust debounce window (default: 2s, supports ms, s, m)
./funk-auto-sync -path /path/to/notes -debounce 500ms
./funk-auto-sync -path /path/to/notes -debounce 5s
./funk-auto-sync -path /path/to/notes -debounce 10s

# Check version
./funk-auto-sync -version

# Show help
./funk-auto-sync -help
```

### Available flags

| Flag | Type | Default | Description |
|---|---|---|---|
| `-path` | string | `.` | Path(s) of the notes directory to watch. Repeatable (`-path a -path b`) or comma-separated (`-path a,b`). |
| `-debounce` | duration | `2s` | How long to wait for changes to settle before syncing (e.g. `500ms`, `5s`, `10s`), coalescing rapid saves into a single commit. |
| `-version` | bool | `false` | Print the version number and exit. |

## How it works

When `funk-auto-sync` starts:

1. **Pre-flight validation**: Checks that `git` is on your `PATH` and verifies each `-path` is an existing Git repository with an `origin` remote.
2. **Recursive watcher**: Recursively registers all subdirectories with `fsnotify`. Any new folders created later are picked up automatically.
3. **Event filtering**: Temporary editor swap files (`*~`, `.*`), office lock files (`~$*`), and files matched by `.gitignore` are immediately skipped.
4. **Per-repo debounce**: Waits for `-debounce` duration without new changes in that repository before triggering the pipeline.
5. **Git Add with secret exclude**: Runs `git add .` with `:(exclude)` pathspecs for common secret files (`.env`, `*.key`, `*.pem`, etc.).
6. **Staged content scan**: Inspects staged files for credentials/private keys. If found, resets staging and aborts for safety.
7. **Descriptive commit**: Generates a commit message summarizing changed files and line additions/deletions (e.g. `Update notes.md (+12/-3); Add report.docx (binary) — 2026-08-31 10:15:00`).
8. **Pull with rebase & retry**: Executes `git pull origin <branch> --rebase` with up to 3 exponential backoff retries for network glitches.
9. **Push with retry**: Executes `git push origin <branch>` with exponential backoff retries.
10. **Desktop notification**: Sends a native desktop alert with the sync result.

## Office & Binary file support

`funk-auto-sync` works seamlessly with binary documents (such as `.docx`, `.xlsx`, `.pptx`, `.pdf`, and images):

- **Lock-file exclusion**: Word and Excel create temporary lock files (e.g. `~$Annual_Report.docx`) during editing. These files are filtered out automatically to avoid commit noise and race conditions.
- **Commit formatting**: Since binary files do not have line diffs, they are formatted as `Update Annual_Report.docx (binary)` instead of misleading `(+0/-0)`.
- **Debounce tip for large files**: Saving large Office documents involves writing temporary files before renaming. If commits trigger prematurely during large saves, increase the debounce delay:
  ```bash
  ./funk-auto-sync -path ~/documents -debounce 5s
  ```

## Watching multiple folders

A single process can watch several independent Git repositories at once — each keeps its own branch, remote, and commit history:

```bash
./funk-auto-sync -path ~/notes -path ~/journal -path ~/dotfiles
```

Log lines and desktop notifications are labeled with the repo's folder name (e.g. `[notes] Change detected: todo.md`).

## Handling conflicts

If `git pull --rebase` fails due to remote conflicts:
1. The tool automatically runs `git rebase --abort` to keep the local repo clean.
2. An error notification is sent.
3. Your local commit is preserved so you can resolve the merge manually.

## Running as a background service (optional)

<details>
<summary><strong>Linux (systemd user service)</strong></summary>

Create `~/.config/systemd/user/funk-auto-sync.service`:

```ini
[Unit]
Description=Funk Auto Sync

[Service]
ExecStart=/absolute/path/to/funk-auto-sync -path /absolute/path/to/notes-folder
Restart=on-failure

[Install]
WantedBy=default.target
```

Enable and start:

```bash
systemctl --user enable --now funk-auto-sync.service
```

</details>

<details>
<summary><strong>macOS (launchd)</strong></summary>

Create `~/Library/LaunchAgents/com.funk.funk-auto-sync.plist` pointing to the compiled binary with the desired `-path` argument, then load it:

```bash
launchctl load ~/Library/LaunchAgents/com.funk.funk-auto-sync.plist
```

</details>

<details>
<summary><strong>Windows (Task Scheduler)</strong></summary>

Use Task Scheduler to run `funk-auto-sync.exe -path "C:\path\to\notes-folder"` at logon.

</details>

## Reducing binary size

Build a stripped, fully static binary without CGO:

```bash
# Linux / macOS
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o funk-auto-sync main.go

# Windows (PowerShell)
$env:CGO_ENABLED=0; go build -trimpath -ldflags="-s -w" -o funk-auto-sync.exe main.go
```

## Development & Tests

Run unit tests locally (test files `*_test.go` are excluded from release commits):

```bash
go test ./... -v
# or
make test
```

## Project structure

```
.
├── main.go        # Application entry point, watcher, and git pipeline
├── main_test.go   # Unit tests (excluded from git via .gitignore)
├── go.mod         # Go module definition
├── Makefile       # Build shortcuts (build, build-all, test, clean)
├── banner.jpg     # README header image
├── .gitignore     # Git ignore rules for open source release
└── README.md      # Documentation
```

## License

MIT — feel free to use, modify, and distribute.
