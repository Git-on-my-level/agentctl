package main

import (
	"context"
	"net/url"
	"regexp"
	"strings"
)

var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// devinCloudRepository returns owner/name of the launch workspace's GitHub
// origin. A Devin cloud session clones that repository on Devin's machines;
// the ACP client then requires it among the session's connected repos. Only
// local git configuration is read, and credentials in the URL are discarded.
func devinCloudRepository(ctx context.Context, cwd string) (string, bool) {
	remote, err := gitRead(ctx, cwd, "remote", "get-url", "origin")
	if err != nil {
		return "", false
	}
	return githubRepository(remote)
}

func githubRepository(remote string) (string, bool) {
	remote = strings.TrimSpace(remote)
	path := ""
	if rest, ok := strings.CutPrefix(remote, "git@github.com:"); ok {
		path = rest
	} else if parsed, err := url.Parse(remote); err == nil && strings.EqualFold(parsed.Hostname(), "github.com") && (parsed.Scheme == "https" || parsed.Scheme == "ssh") {
		path = strings.TrimPrefix(parsed.Path, "/")
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if !githubRepositoryPattern.MatchString(path) {
		return "", false
	}
	return path, true
}
