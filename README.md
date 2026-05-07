# Gitessera

Gitessera implements a storage backend for [Tessera](https://github.com/transparency-dev/tessera) using the GitHub API. It allows you to use a GitHub repository as a [tlog-tiles](https://c2sp.org/tlog-tiles) compliant transparency log.

## How it Works

Gitessera uses the GitHub Git Data API to store log data (checkpoints, tiles, and entry bundles) as Git blobs, trees, and commits. This allows the log state to be persisted in a Git repository branch.

## Usage

### Initializing the Log Branch

Before using Gitessera, you need to create and initialize a dedicated orphan branch to store the log data. We recommend naming it `tlog`.

```bash
# 1. Create a new orphan branch named tlog
git checkout --orphan tlog

# 2. Clear all files from the working tree
git rm -rf .

# 3. Create an empty initial commit
git commit --allow-empty -m "Initialize empty log data branch"

# 4. Push the branch to GitHub
git push origin tlog

# 5. Switch back to your main branch
git checkout main
```

### CLI Tool

The project includes a CLI tool in `cmd/main.go` that adds an entry to the log. The entry content is read from the `ISSUE_BODY` environment variable.

#### Environment Variables

- `GITHUB_TOKEN`: A GitHub Personal Access Token (PAT) or workflow token with permission to write to the repository.
- `LOG_PRIVATE_KEY`: The private key used to sign checkpoints.
- `ISSUE_BODY`: The content of the entry to be added to the log.

#### Flags

- `-owner`: The GitHub repository owner.
- `-repo`: The GitHub repository name.
- `-branch`: The branch to store log data in.
- `-private_key`: (Optional) Location of private key file. If unset, uses `LOG_PRIVATE_KEY` environment variable.
- `-witness_policy_file`: (Optional) Path to the file containing the witness policy.
- `-slog_level`: (Optional) The cut-off threshold for structured logging (default 0 for INFO).

Example usage:

```bash
export GITHUB_TOKEN="your_github_token"
export LOG_PRIVATE_KEY="your_private_key"
export ISSUE_BODY="content to add"

go run ./cmd/main.go -owner roger2hk -repo gitessera -branch tlog
```

### GitHub Actions Workflow

A GitHub Actions workflow is available at `.github/workflows/add_entry.yml` to automate adding entries when an issue is labeled with `add-entry`.

## License

This project is licensed under the Apache License, Version 2.0. See the [LICENSE](LICENSE) file for details.
