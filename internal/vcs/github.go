package vcs

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// github talks to GitHub's REST API (github.com or GitHub Enterprise Server's /api/v3).
type github struct{}

func (github) authorize(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

func (github) etags() bool { return true }

// shortcut: GitHub stops at 1000 repos; type the name if yours isn't listed.
func (github) repos(ctx context.Context, c Conn) ([]string, error) {
	var out []string
	for page := 1; page <= 10; page++ {
		var rs []struct {
			FullName string `json:"full_name"`
		}
		if err := c.getJSON(ctx, c.BaseURL+"/user/repos?per_page=100&sort=full_name&page="+strconv.Itoa(page), &rs, false); err != nil {
			return nil, err
		}
		for _, r := range rs {
			out = append(out, r.FullName)
		}
		if len(rs) < 100 {
			break
		}
	}
	return out, nil
}

func (github) tags(ctx context.Context, c Conn, a, b string) ([]Tag, error) {
	var out []Tag
	for page := 1; page <= 10; page++ { // shortcut: the first 1000 tags GitHub lists (not by date); page through git refs if repos get more
		var ts []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/tags?per_page=100&page=%d", c.BaseURL, a, b, page), &ts, true); err != nil {
			return nil, err
		}
		for _, t := range ts {
			out = append(out, Tag{t.Name, t.Commit.SHA})
		}
		if len(ts) < 100 {
			break
		}
	}
	return out, nil
}

func (github) head(ctx context.Context, c Conn, a, b, branch string) (string, error) {
	var br struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/branches/%s", c.BaseURL, a, b, url.PathEscape(branch)), &br, true)
	return br.Commit.SHA, err
}

// shortcut: the first 100 pull requests; plenty for one branch.
func (github) pullRequests(ctx context.Context, c Conn, repo, a, b, branch string) ([]PullRequest, error) {
	var prs []struct {
		Number  int    `json:"number"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"` // null once a fork is deleted
		} `json:"head"`
	}
	q := url.Values{"state": {"open"}, "base": {branch}, "per_page": {"100"}}
	if err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/pulls?%s", c.BaseURL, a, b, q.Encode()), &prs, true); err != nil {
		return nil, err
	}
	var out []PullRequest
	for _, p := range prs {
		if p.Head.Repo != nil && strings.EqualFold(p.Head.Repo.FullName, repo) && p.Head.SHA != "" {
			out = append(out, PullRequest{Number: p.Number, Commit: p.Head.SHA, Branch: p.Head.Ref, Body: p.Body, URL: p.HTMLURL})
		}
	}
	return out, nil
}

// download: a tarball (any ref, so refType isn't needed) with everything under "<owner>-<repo>-<sha>/".
func (github) download(ctx context.Context, c Conn, a, b, ref, _ string) ([]file, error) {
	raw, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/%s/tarball/%s", c.BaseURL, a, b, url.PathEscape(ref)), maxDownload, false)
	if err != nil {
		return nil, err
	}
	files, err := readTarGz(raw)
	if err != nil {
		return nil, err
	}
	return repoFiles(files, true), nil
}

func (github) createBranch(ctx context.Context, c Conn, a, b, branch, commit string) error {
	return c.send(ctx, http.MethodPost, fmt.Sprintf("%s/repos/%s/%s/git/refs", c.BaseURL, a, b),
		map[string]string{"ref": "refs/heads/" + branch, "sha": commit}, nil)
}

func (github) commitFile(ctx context.Context, c Conn, a, b, branch, base, file string, body []byte, isNew bool, msg string) error {
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s", c.BaseURL, a, b, (&url.URL{Path: file}).EscapedPath())
	req, method := map[string]string{"message": msg, "branch": branch}, http.MethodDelete
	if body != nil {
		req["content"], method = base64.StdEncoding.EncodeToString(body), http.MethodPut
	}
	if !isNew { // the file's blob at base: GitHub answers 409 if the branch has another version by now
		var cur struct {
			SHA string `json:"sha"`
		}
		if err := c.getJSON(ctx, u+"?ref="+url.QueryEscape(base), &cur, false); err != nil {
			return err
		}
		req["sha"] = cur.SHA
	}
	return c.send(ctx, method, u, req, nil)
}

func (github) openPR(ctx context.Context, c Conn, a, b, branch, target, title, body string) (string, error) {
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	err := c.send(ctx, http.MethodPost, fmt.Sprintf("%s/repos/%s/%s/pulls", c.BaseURL, a, b),
		map[string]string{"title": title, "head": branch, "base": target, "body": body}, &pr)
	return pr.HTMLURL, err
}
