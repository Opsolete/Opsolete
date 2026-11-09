package vcs

import "context"

// The writes of the template editor (internal/app's Git catalog): a branch, a one-file commit and a
// pull request.

// CreateBranch creates branch at commit.
func (c Conn) CreateBranch(ctx context.Context, repo, branch, commit string) error {
	p, a, b, err := c.repo(repo)
	if err != nil {
		return err
	}
	return p.createBranch(ctx, c, a, b, branch, commit)
}

// CommitFile commits one file to branch, whose current commit must be base. A nil body deletes the file.
func (c Conn) CommitFile(ctx context.Context, repo, branch, base, file string, body []byte, isNew bool, msg string) error {
	p, a, b, err := c.repo(repo)
	if err != nil {
		return err
	}
	return p.commitFile(ctx, c, a, b, branch, base, file, body, isNew, msg)
}

// OpenPR opens a pull request of branch into target and returns its web URL.
func (c Conn) OpenPR(ctx context.Context, repo, branch, target, title, body string) (string, error) {
	p, a, b, err := c.repo(repo)
	if err != nil {
		return "", err
	}
	return p.openPR(ctx, c, a, b, branch, target, title, body)
}
