package cleanup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/cli/cli/v2/api"
	cliContext "github.com/cli/cli/v2/context"
	"github.com/cli/cli/v2/git"
	"github.com/cli/cli/v2/internal/config"
	"github.com/cli/cli/v2/internal/prompter"
	"github.com/cli/cli/v2/internal/tableprinter"
	"github.com/cli/cli/v2/pkg/cmd/pr/shared"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/spf13/cobra"
)

type CleanupOptions struct {
	HttpClient func() (*http.Client, error)
	GitClient  *git.Client
	Config     func() (config.Config, error)
	IO         *iostreams.IOStreams
	Remotes    func() (cliContext.Remotes, error)
	Branch     func() (string, error)
	Prompter   prompter.Prompter

	Finder shared.PRFinder

	SelectorArg  string
	All          bool
	Strict       bool
	MergedOnly   bool
	UpToDateOnly bool
	Yes          bool
}

func NewCmdCleanup(f *cmdutil.Factory, runF func(*CleanupOptions) error) *cobra.Command {
	opts := &CleanupOptions{
		IO:         f.IOStreams,
		HttpClient: f.HttpClient,
		GitClient:  f.GitClient,
		Config:     f.Config,
		Remotes:    f.Remotes,
		Branch:     f.Branch,
		Prompter:   f.Prompter,
	}

	cmd := &cobra.Command{
		Use:   "cleanup {<number> | <url> | <branch> | --all}",
		Short: "Clean up local branches of merged or closed pull requests",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Finder = shared.NewFinder(f)

			if len(args) > 0 {
				opts.SelectorArg = args[0]
			}

			if runF != nil {
				return runF(opts)
			}
			return cleanupRun(opts)
		},
	}

	cmd.Flags().BoolVarP(&opts.All, "all", "", false, "Clean up all merged pull requests")
	cmd.Flags().BoolVarP(&opts.Strict, "strict", "", false, "Both --exclude-closed and --exclude-behind")
	cmd.Flags().BoolVarP(&opts.MergedOnly, "exclude-closed", "", false, "Exclude branches of closed pull requests")
	cmd.Flags().BoolVarP(&opts.UpToDateOnly, "exclude-behind", "", false, "Exclude branches that are behind their remote")
	cmd.Flags().BoolVarP(&opts.Yes, "yes", "", false, "Skip deletion confirmation")

	return cmd
}

func cleanupRun(opts *CleanupOptions) error {
	// Validate input arguments: --all and PR selector are mutually exclusive, but
	// at least one must be set.
	if opts.All && opts.SelectorArg != "" {
		return errors.New("Invalid arguments: cannot set both PR and --all")
	} else if opts.SelectorArg == "" && !opts.All {
		return errors.New("Invalid arguments: must set either PR or --all")
	}

	// Set flags.
	if opts.Strict {
		opts.MergedOnly = true
		opts.UpToDateOnly = true
	}

	if opts.All {
		// Get all local branches and their upstreams.
		ctx := context.Background()
		localBranches := opts.GitClient.LocalBranches(ctx)
		var branchesWithUpstream []git.Branch
		for _, localBranch := range localBranches {
			if localBranch.Upstream.RemoteName == "" {
				continue
			}
			branchesWithUpstream = append(branchesWithUpstream, localBranch)
		}

		timeWarning := ".."
		if len(branchesWithUpstream) > 60 {
			timeWarning = " This might take a few minutes..."
		} else if len(branchesWithUpstream) > 30 {
			timeWarning = " This might take a minute..."
		} else if len(branchesWithUpstream) > 10 {
			timeWarning = " This might take a few seconds..."
		}
		opts.IO.StartProgressIndicatorWithLabel(
			fmt.Sprintf(
				"Loading PRs for %d local branches with upstreams.%s\n",
				len(branchesWithUpstream),
				timeWarning,
			),
		)

		// Get PRs associated with upstream branches.
		var prs []*api.PullRequest
		// TODO: Can these be loaded in parallel?
		for _, branch := range branchesWithUpstream {
			// TODO: This causes the progress indicator to "reset" very frequently.
			// Should the Finder itself have a progress indicator? Perhaps we should
			// invert that so consumers have control of the indicator instead.
			pr, _, err := opts.Finder.Find(shared.FindOptions{
				Selector: branch.Upstream.BranchName,
				Fields:   []string{"commits", "headRefOid", "title"},
				States:   []string{"MERGED", "CLOSED"},
			})
			if _, ok := err.(*shared.NotFoundError); ok {
				continue
			}
			if err != nil {
				return err
			} else {
				prs = append(prs, pr)

				// Avoid rate limit. Since rate-limiting is based on count of nodes
				// loaded, we only need to worry about it in the case where finding a PR
				// succeeded (because no nodes are loaded in the not-found case).
				//
				// TODO: Intelligently retry on rate limiting instead.
				time.Sleep(time.Second)
			}
		}
		opts.IO.StopProgressIndicator()

		// Reorganize branches by their HEAD commits for fast lookup.
		branchesByCommit := make(map[string][]git.Branch)
		for _, branch := range branchesWithUpstream {
			branchesByCommit[branch.Local.Hash] = append(branchesByCommit[branch.Local.Hash], branch)
		}

		// Get the list of candidate branch deletions.
		//
		// Any local branch whose HEAD is a commit of a merged or closed PR is a
		// candidate for deletion, because the local branch's history is a prefix of
		// the remote branch's history (i.e. there are no local commits that the
		// upstream does not have).
		//
		// This behavior is altered by:
		// * --exclude-behind: the local branch's head ref must be the PR's head ref.
		// * --exclude-closed: closed PRs are not considered.
		deletionCandidates := make(map[git.Branch]*api.PullRequest)
		for _, pr := range prs {
			if opts.MergedOnly && pr.State == "CLOSED" {
				continue
			}

			if opts.UpToDateOnly {
				candidates := branchesByCommit[pr.HeadRefOid]
				for _, candidate := range candidates {
					deletionCandidates[candidate] = pr
				}
			} else {
				for _, commit := range pr.Commits.Nodes {
					candidates := branchesByCommit[commit.Commit.OID]
					for _, candidate := range candidates {
						deletionCandidates[candidate] = pr
					}
				}
			}
		}

		// Interactively confirm branch deletion.
		cs := opts.IO.ColorScheme()
		if len(deletionCandidates) == 0 {
			fmt.Fprintf(opts.IO.Out, "%s No branches to be cleaned up!\n", cs.SuccessIcon())
			return nil
		}

		var branchesInAlphaOrder []git.Branch
		for branch := range deletionCandidates {
			branchesInAlphaOrder = append(branchesInAlphaOrder, branch)
		}
		sort.Slice(branchesInAlphaOrder, func(i, j int) bool {
			return branchesInAlphaOrder[i].Local.Name < branchesInAlphaOrder[j].Local.Name
		})

		fmt.Fprintf(opts.IO.Out, "\nThe following branches can be cleaned up:\n\n")
		table := tableprinter.New(opts.IO)
		table.HeaderRow("Branch", "Status", "Pull Request")
		for _, branch := range branchesInAlphaOrder {
			pr := deletionCandidates[branch]

			table.AddField(branch.Local.Name)

			state := pr.State
			if branch.Local.Hash != pr.HeadRefOid {
				state = cs.WarningIcon() + " " + cs.Yellow(state)
			}
			if state == "MERGED" {
				state = cs.SuccessIcon() + " " + cs.Green(state)
			} else if state == "CLOSED" {
				state = cs.SuccessIcon() + " " + cs.Red(state)
			}
			table.AddField(state)

			table.AddField(
				fmt.Sprintf(
					"%s %s",
					cs.Grayf("#%d", pr.Number),
					pr.Title,
				),
			)

			table.EndRow()
		}
		err := table.Render()
		if err != nil {
			return err
		}

		if !opts.UpToDateOnly {
			fmt.Fprintf(opts.IO.Out, "\n%s indicates that a local branch is behind its remote.\n", cs.WarningIcon())
		}
		fmt.Fprintf(opts.IO.Out, "\n")

		confirmed := false
		if opts.Yes {
			confirmed = true
		} else if opts.IO.CanPrompt() {
			branchTypeStr := "merged or closed"
			if opts.MergedOnly {
				branchTypeStr = "merged"
			}
			confirmed, err = opts.Prompter.Confirm(
				fmt.Sprintf("Delete all %d %s branches?", len(deletionCandidates), branchTypeStr),
				false,
			)
			if err != nil {
				return err
			}
		}

		// Delete branches.
		if confirmed {
			for branch := range deletionCandidates {
				err := opts.GitClient.DeleteLocalBranch(ctx, branch.Local.Name)
				if err != nil {
					return err
				}
			}
			fmt.Fprintf(opts.IO.Out, "Deleted %d branches.\n", len(deletionCandidates))
		} else {
			fmt.Fprintf(opts.IO.Out, "Not deleting any branches.\n")
		}
	}

	return nil
}