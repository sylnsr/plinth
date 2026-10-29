package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"

	"plinth/internal/names"
)

// Posted is an issue or a comment Plinth posted: its id, its number (an issue's), and its page.
type Posted struct {
	ID     int64  `json:"id"`
	Number int64  `json:"number"`
	URL    string `json:"html_url"`
}

// CreateIssue opens an issue in repo.
func (c *Client) CreateIssue(ctx context.Context, repo names.Namespace, title, body string) (Posted, error) {
	return c.postJSON(ctx, "/repos/"+repo.Owner+"/"+repo.Repo+"/issues", map[string]string{"title": title, "body": body})
}

// Comment posts a comment on issue (its number) of repo.
func (c *Client) Comment(ctx context.Context, repo names.Namespace, issue, body string) (Posted, error) {
	if !names.ValidIssue(issue) {
		return Posted{}, fmt.Errorf("issue %q: not an issue number", issue)
	}
	return c.postJSON(ctx, "/repos/"+repo.Owner+"/"+repo.Repo+"/issues/"+issue+"/comments", map[string]string{"body": body})
}

// AttachToIssue attaches a file named name to issue (its number) of repo.
func (c *Client) AttachToIssue(ctx context.Context, repo names.Namespace, issue int64, name string, data []byte) error {
	return c.attach(ctx, "/repos/"+repo.Owner+"/"+repo.Repo+"/issues/"+strconv.FormatInt(issue, 10)+"/assets", name, data)
}

// AttachToComment attaches a file named name to the comment (its id) in repo.
func (c *Client) AttachToComment(ctx context.Context, repo names.Namespace, comment int64, name string, data []byte) error {
	return c.attach(ctx, "/repos/"+repo.Owner+"/"+repo.Repo+"/issues/comments/"+strconv.FormatInt(comment, 10)+"/assets", name, data)
}

func (c *Client) postJSON(ctx context.Context, path string, in any) (Posted, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return Posted{}, err
	}
	var p Posted
	if err := c.do(ctx, http.MethodPost, path, "application/json", bytes.NewReader(b), &p); err != nil {
		return Posted{}, err
	}
	return p, nil
}

// attach uploads data as the form field attachment, as Forgejo's asset endpoints want it.
func (c *Client) attach(ctx context.Context, path, name string, data []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("attachment", name)
	if err == nil {
		_, err = part.Write(data)
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	return c.do(ctx, http.MethodPost, path+"?name="+url.QueryEscape(name), w.FormDataContentType(), &buf, &out)
}
