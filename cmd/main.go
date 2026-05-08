package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/oauth2"

	"log/slog"

	"github.com/google/go-github/v60/github"
	"github.com/roger2hk/gitessera"
	"github.com/transparency-dev/tessera"
)

var (
	owner             = flag.String("owner", "", "The GitHub repository owner.")
	repo              = flag.String("repo", "", "The GitHub repository name.")
	branch            = flag.String("branch", "", "The branch to store log data in.")
	privKeyFile       = flag.String("private_key", "", "Location of private key file. If unset, uses the contents of the LOG_PRIVATE_KEY environment variable.")
	witnessPolicyFile = flag.String("witness_policy_file", "", "(Optional) Path to the file containing the witness policy in the format describe at https://git.glasklar.is/sigsum/core/sigsum-go/-/blob/main/doc/policy.md")
	witnessTimeout    = flag.Duration("witness_timeout", tessera.DefaultWitnessTimeout, "Maximum time to wait for witness responses.")
	witnessFailOpen   = flag.Bool("witness_fail_open", false, "Still publish a checkpoint even if witness policy could not be met")
	slogLevel         = flag.Int("slog_level", 0, "The cut-off threshold for structured logging. Default is 0 (INFO). See https://pkg.go.dev/log/slog#Level for other levels.")
)

// entryInfo binds the actual bytes to be added as a leaf with a
// user-recognisable name for the source of those bytes.
// The name is only used below in order to inform the user of the
// sequence numbers assigned to the data from the provided input files.
type entryInfo struct {
	name string
	f    tessera.IndexFuture
}

func main() {
	flag.Parse()
	ctx := context.Background()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.Level(*slogLevel)})))

	slog.DebugContext(ctx, "Initialising driver")

	// Gather the info needed for reading/writing checkpoints
	s := getSignerOrDie()

	// Authenticate to GitHub
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		slog.ErrorContext(ctx, "GITHUB_TOKEN environment variable not set")
		os.Exit(1)
	}
	ts := oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	))
	client := github.NewClient(ts)

	driver := gitessera.NewGitHubStorage(client, *owner, *repo, *branch)

	slog.DebugContext(ctx, "Reading entry from ISSUE_BODY")
	issueBody := os.Getenv("ISSUE_BODY")
	if issueBody == "" {
		slog.ErrorContext(ctx, "ISSUE_BODY environment variable not set")
		os.Exit(1)
	}

	slog.DebugContext(ctx, "Configuring options")
	opts := tessera.NewAppendOptions().
		WithCheckpointSigner(s).
		WithBatching(1, 100*time.Millisecond).
		WithCheckpointInterval(100 * time.Millisecond)

	if *witnessPolicyFile != "" {
		f, err := os.ReadFile(*witnessPolicyFile)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to read witness policy file", slog.String("witnesspolicyfile", *witnessPolicyFile), slog.Any("error", err))
			os.Exit(1)
		}
		wg, err := tessera.NewWitnessGroupFromPolicy(f)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to create witness group from policy", slog.Any("error", err))
			os.Exit(1)
		}

		wOpts := &tessera.WitnessOptions{
			FailOpen: *witnessFailOpen,
			Timeout:  *witnessTimeout,
		}
		opts.WithWitnesses(wg, wOpts)
	}

	slog.DebugContext(ctx, "Creating appender")
	appender, shutdown, r, err := tessera.NewAppender(ctx, driver, opts)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create new appender", slog.Any("error", err))
		os.Exit(1)
	}

	slog.DebugContext(ctx, "Creating awaiter")
	await := tessera.NewPublicationAwaiter(ctx, r.ReadCheckpoint, 100*time.Millisecond)

	slog.DebugContext(ctx, "Adding entry")
	f := appender.Add(ctx, tessera.NewEntry([]byte(issueBody)))
	indexFutures := []entryInfo{{name: "ISSUE_BODY", f: f}}

	slog.DebugContext(ctx, "Awaiting entry")
	// Two options to ensure all work is done:
	// 1) Check each of the futures to ensure that the leaves are sequenced.
	for _, entry := range indexFutures {
		seq, _, err := await.Await(ctx, entry.f)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to sequence", slog.String("name", entry.name), slog.Any("error", err))
			os.Exit(1)
		}
		slog.InfoContext(ctx, "Integrated entry", slog.Uint64("index", seq.Index), slog.String("name", entry.name))

		// Write to GITHUB_OUTPUT if available
		if githubOutput := os.Getenv("GITHUB_OUTPUT"); githubOutput != "" {
			f, err := os.OpenFile(githubOutput, os.O_APPEND|os.O_WRONLY, 0644)
			if err != nil {
				slog.ErrorContext(ctx, "Failed to open GITHUB_OUTPUT file", slog.Any("error", err))
			} else {
				defer f.Close()
				if _, err := fmt.Fprintf(f, "seq_index=%d\n", seq.Index); err != nil {
					slog.ErrorContext(ctx, "Failed to write to GITHUB_OUTPUT", slog.Any("error", err))
				}
			}
		}
	}
	slog.DebugContext(ctx, "Futures resolved")
	slog.DebugContext(ctx, "Shutting down")

	// 2) shutdown the appender
	if err := shutdown(ctx); err != nil {
		slog.ErrorContext(ctx, "Failed to shut down cleanly", slog.Any("error", err))
		os.Exit(1)
	}
	slog.DebugContext(ctx, "Finished")
}

// Read log private key from file or environment variable
func getSignerOrDie() note.Signer {
	var privKey string
	var err error
	if len(*privKeyFile) > 0 {
		privKey, err = getKeyFile(*privKeyFile)
		if err != nil {
			slog.ErrorContext(context.Background(), "Unable to get private key", slog.Any("error", err))
			os.Exit(1)
		}
	} else {
		privKey = os.Getenv("LOG_PRIVATE_KEY")
		if len(privKey) == 0 {
			slog.ErrorContext(context.Background(), "Supply private key file path using --private_key or set LOG_PRIVATE_KEY environment variable")
			os.Exit(1)
		}
	}
	s, err := note.NewSigner(privKey)
	if err != nil {
		slog.ErrorContext(context.Background(), "Failed to instantiate signer", slog.Any("error", err))
		os.Exit(1)
	}
	return s
}

func getKeyFile(path string) (string, error) {
	k, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read key file: %w", err)
	}
	return string(k), nil
}


