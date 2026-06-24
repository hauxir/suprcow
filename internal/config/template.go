package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// prToken is the placeholder in subdomain patterns replaced by the PR number.
const prToken = "{n}"

// RenderContext resolves the per-environment template variables that appear in
// inject env values and file contents. It is the mechanism that lets one
// service learn another's *external* preview URL (the generic version of
// "the frontend needs the backend's host").
//
// Supported variables:
//
//	${PR_NUMBER}            the pull request number
//	${BRANCH}               the PR head branch
//	${SHA}                  the commit SHA being run
//	${PREVIEW_HOST(svc)}    bare external host of an exposed service
//	${PREVIEW_URL(svc)}     https URL of an exposed service
type RenderContext struct {
	PRNumber   int
	Branch     string
	SHA        string
	BaseDomain string

	expose map[string]ExposeRule
}

// RenderContext builds a resolver for a specific PR environment. baseDomain is
// the wildcard root, e.g. "preview.example.com".
func (c *Config) RenderContext(prNumber int, branch, sha, baseDomain string) *RenderContext {
	exposeByService := make(map[string]ExposeRule, len(c.Expose))
	for _, e := range c.Expose {
		exposeByService[e.Service] = e
	}
	return &RenderContext{
		PRNumber:   prNumber,
		Branch:     branch,
		SHA:        sha,
		BaseDomain: baseDomain,
		expose:     exposeByService,
	}
}

// Host returns the external host for an exposed service, e.g. "api-pr-123.preview.example.com".
func (rc *RenderContext) Host(service string) (string, error) {
	e, ok := rc.expose[service]
	if !ok {
		return "", fmt.Errorf("service %q is not exposed (no expose rule), cannot resolve its preview host", service)
	}
	sub := strings.ReplaceAll(e.Subdomain, prToken, strconv.Itoa(rc.PRNumber))
	if rc.BaseDomain == "" {
		return sub, nil
	}
	return sub + "." + rc.BaseDomain, nil
}

var varPattern = regexp.MustCompile(`\$\{\s*([A-Z_]+)(?:\(\s*([a-zA-Z0-9_.-]+)\s*\))?\s*\}`)

// Resolve replaces every ${...} variable in s. It returns an error if any
// variable is unknown or references a non-exposed service, so misconfiguration
// surfaces at spawn time rather than as a silent empty value.
func (rc *RenderContext) Resolve(s string) (string, error) {
	var firstErr error
	out := varPattern.ReplaceAllStringFunc(s, func(match string) string {
		groups := varPattern.FindStringSubmatch(match)
		name, arg := groups[1], groups[2]
		val, err := rc.resolveVar(name, arg)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return val
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

func (rc *RenderContext) resolveVar(name, arg string) (string, error) {
	switch name {
	case "PR_NUMBER":
		return strconv.Itoa(rc.PRNumber), nil
	case "BRANCH":
		return rc.Branch, nil
	case "SHA":
		return rc.SHA, nil
	case "PREVIEW_HOST":
		if arg == "" {
			return "", fmt.Errorf("PREVIEW_HOST requires a service argument, e.g. ${PREVIEW_HOST(api)}")
		}
		return rc.Host(arg)
	case "PREVIEW_URL":
		if arg == "" {
			return "", fmt.Errorf("PREVIEW_URL requires a service argument, e.g. ${PREVIEW_URL(api)}")
		}
		host, err := rc.Host(arg)
		if err != nil {
			return "", err
		}
		return "https://" + host, nil
	default:
		return "", fmt.Errorf("unknown template variable ${%s}", name)
	}
}

// ResolveEnv resolves every value in a service's inject env map.
func (rc *RenderContext) ResolveEnv(env map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(env))
	for k, v := range env {
		resolved, err := rc.Resolve(v)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		out[k] = resolved
	}
	return out, nil
}
