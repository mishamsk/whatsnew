# whatsnew

> [!WARNING]
> This was vibe coded for personal use. It has not been carefully verified, hardened, or tested. Use it at your own risk.

`whatsnew` is a tiny TUI for reading changelogs for tools you already have installed.

I am just always curious what's new in changelogs and got fed up manually jumping to repos to read them. This tool asks Homebrew what is outdated, asks `mise ls` what is active, resolves GitHub repositories where it can, and shows the latest release notes in a simple terminal UI.

## What It Does

- Shows Homebrew outdated tools, or recently updated Homebrew tools if nothing is outdated.
- Shows active tools from `mise ls`.
- Fetches GitHub release notes, then falls back to `CHANGELOG.md` in the repo root.
- Caches resolved repos and fetched changelogs under `$XDG_CACHE_DIR/whatsnew`.
- Uses `gh` for GitHub auth when available, then `GITHUB_TOKEN`, then unauthenticated API calls.

Tools without a GitHub repo or changelog are hidden by default. Press `h` in the UI to show them.

## Requirements

- Go, managed here with `mise`
- Optional: `brew`
- Optional: `mise`
- Optional: `gh` or `GITHUB_TOKEN` for better GitHub rate limits

If `brew` or `mise` is missing, that source is skipped.

## Usage

```sh
just setup
just run
```

Or build a binary:

```sh
just build
./bin/whatsnew
```

## Keys

- `up/down`: scroll tools
- `pgup/pgdn`: scroll changelog view
- `/`: search tools
- `h`: show or hide tools without changelogs
- `q`: quit
