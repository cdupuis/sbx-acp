package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// mixinNamespace is where sbx resolves built-in agent names, so it is the one
// namespace whose kits are built against the agents sbx-acp creates.
const mixinNamespace = "sbx"

const (
	kitMediaType  = "application/vnd.docker.sandbox.kit.v3"
	descriptorKey = "vnd.docker.sandbox.kit.descriptor"
	maxBody       = 1 << 20
)

var repoName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// catalog reads published kits through the documented Docker Hub API and
// anonymous registry pulls. It never decodes a descriptor.
type catalog struct {
	hub, auth, registry string
	client              *http.Client
}

func dockerHub() catalog {
	return catalog{
		hub:      "https://hub.docker.com",
		auth:     "https://auth.docker.io",
		registry: "https://registry-1.docker.io",
		client: &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("refusing a redirect to %s", req.URL.Host)
			}
			return nil
		}},
	}
}

// Published returns the kit descriptors of the repositories in namespace whose
// names name agent and an ACP adapter. Only a failure to list is an error; a
// repository that cannot be read comes back with Unread set.
func (c catalog) Published(ctx context.Context, namespace, agent string) ([]PublishedKit, error) {
	var list struct {
		Next    *string `json:"next"`
		Results []struct {
			Name       string   `json:"name"`
			MediaTypes []string `json:"media_types"`
		} `json:"results"`
	}
	q := url.Values{"name": {"acp"}, "page_size": {"100"}}
	if err := c.get(ctx, c.hub+"/v2/namespaces/"+namespace+"/repositories?"+q.Encode(), nil, &list); err != nil {
		return nil, err
	}
	if list.Next != nil && *list.Next != "" {
		return nil, errors.New("the repository list has more than one page")
	}
	var out []PublishedKit
	for _, r := range list.Results {
		if !repoName.MatchString(r.Name) || !strings.Contains("-"+r.Name+"-", "-"+agent+"-") || !slices.Contains(r.MediaTypes, kitMediaType) {
			continue
		}
		repo := namespace + "/" + r.Name
		descriptor, err := c.descriptor(ctx, repo)
		switch {
		case err != nil:
			out = append(out, PublishedKit{Repo: repo, Unread: oneLine(err)})
		case descriptor != "":
			out = append(out, PublishedKit{Repo: repo, Descriptor: []byte(descriptor)})
		}
	}
	return out, nil
}

// descriptor returns the kit descriptor annotation of repo's latest tag, or ""
// when it has none.
func (c catalog) descriptor(ctx context.Context, repo string) (string, error) {
	var token struct {
		Token string `json:"token"`
	}
	q := url.Values{"service": {"registry.docker.io"}, "scope": {"repository:" + repo + ":pull"}}
	if err := c.get(ctx, c.auth+"/token?"+q.Encode(), nil, &token); err != nil {
		return "", err
	}
	var index struct {
		Annotations map[string]string `json:"annotations"`
	}
	header := http.Header{
		"Authorization": {"Bearer " + token.Token},
		"Accept":        {"application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json"},
	}
	if err := c.get(ctx, c.registry+"/v2/"+repo+"/manifests/latest", header, &index); err != nil {
		return "", err
	}
	return index.Annotations[descriptorKey], nil
}

func (c catalog) get(ctx context.Context, rawURL string, header http.Header, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", req.URL.Redacted(), resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return fmt.Errorf("GET %s: the response is larger than 1 MiB", req.URL.Redacted())
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GET %s: %v", req.URL.Redacted(), err)
	}
	return nil
}
