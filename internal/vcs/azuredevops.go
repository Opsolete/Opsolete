package vcs

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// azureDevOps talks to an Azure DevOps organization's REST API (BaseURL https://dev.azure.com/<org>).
type azureDevOps struct{}

func (azureDevOps) authorize(req *http.Request, token string) {
	req.SetBasicAuth("", token) // PAT as the password
}

func (azureDevOps) etags() bool { return false }

func (azureDevOps) repos(ctx context.Context, c Conn) ([]string, error) {
	var rs struct {
		Value []struct {
			Name    string `json:"name"`
			Project struct {
				Name string `json:"name"`
			} `json:"project"`
		} `json:"value"`
	}
	if err := c.getJSON(ctx, c.BaseURL+"/_apis/git/repositories?api-version=7.1", &rs, false); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rs.Value {
		out = append(out, r.Project.Name+"/"+r.Name)
	}
	return out, nil
}

func (azureDevOps) tags(ctx context.Context, c Conn, a, b string) ([]Tag, error) {
	var rs struct {
		Value []struct {
			Name           string `json:"name"`
			ObjectID       string `json:"objectId"`
			PeeledObjectID string `json:"peeledObjectId"` // the commit of an annotated tag
		} `json:"value"`
	}
	if err := c.getJSON(ctx, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/refs?filter=tags/&peelTags=true&api-version=7.1", c.BaseURL, a, b), &rs, true); err != nil {
		return nil, err
	}
	var out []Tag
	for _, r := range rs.Value {
		commit := r.PeeledObjectID
		if commit == "" {
			commit = r.ObjectID
		}
		out = append(out, Tag{strings.TrimPrefix(r.Name, "refs/tags/"), commit})
	}
	return out, nil
}

func (azureDevOps) head(ctx context.Context, c Conn, a, b, branch string) (string, error) {
	var rs struct {
		Value []struct {
			Name     string `json:"name"`
			ObjectID string `json:"objectId"`
		} `json:"value"`
	}
	q := url.Values{"filter": {"heads/" + branch}, "api-version": {"7.1"}}
	if err := c.getJSON(ctx, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/refs?%s", c.BaseURL, a, b, q.Encode()), &rs, true); err != nil {
		return "", err
	}
	for _, r := range rs.Value { // the filter is a prefix match
		if r.Name == "refs/heads/"+branch {
			return r.ObjectID, nil
		}
	}
	return "", nil
}

// shortcut: the first 100 pull requests; plenty for one branch.
func (azureDevOps) pullRequests(ctx context.Context, c Conn, _, a, b, branch string) ([]PullRequest, error) {
	var rs struct {
		Value []struct {
			ID          int    `json:"pullRequestId"`
			SourceRef   string `json:"sourceRefName"`
			Description string `json:"description"`
			Source      struct {
				CommitID string `json:"commitId"`
			} `json:"lastMergeSourceCommit"`
			ForkSource *struct{} `json:"forkSource"`
		} `json:"value"`
	}
	q := url.Values{"searchCriteria.status": {"active"}, "searchCriteria.targetRefName": {"refs/heads/" + branch},
		"$top": {"100"}, "api-version": {"7.1"}}
	if err := c.getJSON(ctx, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/pullrequests?%s", c.BaseURL, a, b, q.Encode()), &rs, true); err != nil {
		return nil, err
	}
	var out []PullRequest
	for _, p := range rs.Value {
		if p.ForkSource == nil && p.Source.CommitID != "" {
			out = append(out, PullRequest{Number: p.ID, Commit: p.Source.CommitID, Branch: strings.TrimPrefix(p.SourceRef, "refs/heads/"),
				Body: p.Description, URL: azurePRURL(c.BaseURL, a, b, p.ID)})
		}
	}
	return out, nil
}

// download: a zip of the repository root, without a top directory.
func (azureDevOps) download(ctx context.Context, c Conn, a, b, ref, refType string) ([]file, error) {
	// a zip of a folder path is always recursive; Azure rejects recursionLevel alongside path
	q := url.Values{"path": {"/"}, "$format": {"zip"}, "download": {"true"},
		"versionDescriptor.version": {ref}, "versionDescriptor.versionType": {refType}, "api-version": {"7.1"}}
	raw, err := c.get(ctx, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/items?%s", c.BaseURL, a, b, q.Encode()), maxDownload, false)
	if err != nil {
		return nil, err
	}
	files, err := readZip(raw)
	if err != nil {
		return nil, err
	}
	return repoFiles(files, false), nil
}

const zeroCommit = "0000000000000000000000000000000000000000"

func (azureDevOps) createBranch(ctx context.Context, c Conn, a, b, branch, commit string) error {
	var res struct {
		Value []struct {
			Success      bool   `json:"success"`
			UpdateStatus string `json:"updateStatus"`
		} `json:"value"`
	}
	err := c.send(ctx, http.MethodPost, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/refs?api-version=7.1", c.BaseURL, a, b),
		[]map[string]string{{"name": "refs/heads/" + branch, "oldObjectId": zeroCommit, "newObjectId": commit}}, &res)
	if err == nil && (len(res.Value) != 1 || !res.Value[0].Success) {
		err = fmt.Errorf("azure-devops: branch not created: %+v", res.Value)
	}
	return err
}

func (azureDevOps) commitFile(ctx context.Context, c Conn, a, b, branch, base, file string, body []byte, isNew bool, msg string) error {
	change := map[string]any{"changeType": "edit", "item": map[string]string{"path": "/" + file},
		"newContent": map[string]string{"content": string(body), "contentType": "rawtext"}}
	if isNew {
		change["changeType"] = "add"
	}
	if body == nil {
		change["changeType"] = "delete"
		delete(change, "newContent")
	}
	// oldObjectId: Azure DevOps refuses the push (409) if the branch moved since base.
	return c.send(ctx, http.MethodPost, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/pushes?api-version=7.1", c.BaseURL, a, b), map[string]any{
		"refUpdates": []map[string]string{{"name": "refs/heads/" + branch, "oldObjectId": base}},
		"commits":    []map[string]any{{"comment": msg, "changes": []map[string]any{change}}},
	}, nil)
}

func (azureDevOps) openPR(ctx context.Context, c Conn, a, b, branch, target, title, body string) (string, error) {
	var pr struct {
		ID int `json:"pullRequestId"`
	}
	err := c.send(ctx, http.MethodPost, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/pullrequests?api-version=7.1", c.BaseURL, a, b),
		map[string]string{"sourceRefName": "refs/heads/" + branch, "targetRefName": "refs/heads/" + target, "title": title, "description": body}, &pr)
	return azurePRURL(c.BaseURL, a, b, pr.ID), err
}

func azurePRURL(base, project, repo string, id int) string {
	return fmt.Sprintf("%s/%s/_git/%s/pullrequest/%d", base, project, repo, id) // project, repo: escaped by splitRepo
}
