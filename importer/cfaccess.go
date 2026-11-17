package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Cloudflare Access import (P2-03). Codigate policies only allow: everyone, email addresses,
// email domains or IdP groups, any of which lets a person in. A Cloudflare Access application
// whose rules can't be expressed that way without letting more people in (require, exclude,
// deny or bypass policies) is not mapped; the app then stays protected and not live until its
// access is set by hand. Include rules Codigate can't check (IP ranges, countries, …) are dropped,
// which only lets fewer people in, and reported.

// Policy is a Codigate access policy, as PUT /api/v1/apps/{id}/access takes it.
type Policy struct {
	Everyone     bool     `json:"everyone"`
	Emails       []string `json:"emails"`
	EmailDomains []string `json:"email_domains"`
	Groups       []string `json:"groups"`
}

func (p *Policy) empty() bool {
	return !p.Everyone && len(p.Emails) == 0 && len(p.EmailDomains) == 0 && len(p.Groups) == 0
}

// AccessMapping is the outcome for one hostname. Policy is nil when nothing safe was found.
type AccessMapping struct {
	Hostname string
	App      string // Cloudflare Access application name, if one covers the hostname
	Policy   *Policy
	Notes    []Note
}

// CFAccessApp is the part of a Cloudflare Access application the import reads.
type CFAccessApp struct {
	Name              string   `json:"name"`
	Type              string   `json:"type"`
	Domain            string   `json:"domain"`
	SelfHostedDomains []string `json:"self_hosted_domains"`
	Destinations      []struct {
		Type string `json:"type"`
		URI  string `json:"uri"`
	} `json:"destinations"`
	Policies []CFAccessPolicy `json:"policies"`
}

// CFAccessPolicy is an Access policy or an Access group (which has the same three rule lists).
type CFAccessPolicy struct {
	Name     string            `json:"name"`
	Decision string            `json:"decision"`
	Include  []json.RawMessage `json:"include"`
	Exclude  []json.RawMessage `json:"exclude"`
	Require  []json.RawMessage `json:"require"`
}

// AccessApps lists the account's Access applications.
func (c *CloudflareAPI) AccessApps(ctx context.Context, account string) ([]CFAccessApp, error) {
	if !cfAccountPattern.MatchString(account) {
		return nil, fmt.Errorf("cloudflare: invalid account id")
	}
	var all []CFAccessApp
	for page := 1; page <= 50; page++ {
		var apps []CFAccessApp
		if err := c.get(ctx, "/accounts/"+account+"/access/apps?"+url.Values{"page": {strconv.Itoa(page)}, "per_page": {"100"}}.Encode(), &apps); err != nil {
			return nil, err
		}
		all = append(all, apps...)
		if len(apps) < 100 {
			break
		}
	}
	return all, nil
}

// AccessGroup reads an Access group by id.
func (c *CloudflareAPI) AccessGroup(ctx context.Context, account, id string) (*CFAccessPolicy, error) {
	if !cfAccountPattern.MatchString(account) || !cfTunnelPattern.MatchString(id) {
		return nil, fmt.Errorf("cloudflare: invalid account or group id")
	}
	var g CFAccessPolicy
	if err := c.get(ctx, "/accounts/"+account+"/access/groups/"+id, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// GroupLookup resolves an Access group id; MapAccess calls it for "group" rules.
type GroupLookup func(id string) (*CFAccessPolicy, error)

// MapAccess maps the Access applications onto the hostnames being imported.
func MapAccess(apps []CFAccessApp, hostnames []string, groups GroupLookup) []AccessMapping {
	out := make([]AccessMapping, 0, len(hostnames))
	for _, h := range hostnames {
		out = append(out, mapHost(apps, strings.ToLower(h), groups))
	}
	return out
}

func mapHost(apps []CFAccessApp, host string, groups GroupLookup) AccessMapping {
	m := AccessMapping{Hostname: host}
	note := func(level, format string, args ...any) {
		m.Notes = append(m.Notes, Note{Level: level, Rule: -1, Message: fmt.Sprintf(format, args...)})
	}
	var exact, wild *CFAccessApp
	var pathScoped []string
	for i := range apps {
		a := &apps[i]
		for _, d := range accessDomains(a) {
			dh, path, _ := strings.Cut(strings.ToLower(d), "/")
			if path != "" && path != "*" {
				if dh == host || wildcardMatch(dh, host) {
					pathScoped = append(pathScoped, a.Name+" (/"+path+")")
				}
				continue
			}
			if dh == host && exact == nil {
				exact = a
			} else if wildcardMatch(dh, host) && wild == nil {
				wild = a
			}
		}
	}
	app := exact
	if app == nil {
		app = wild
	}
	if len(pathScoped) > 0 {
		sort.Strings(pathScoped)
		note(Warn, "Access applications for paths on this hostname (%s) are not imported; Codigate applies one policy to the whole hostname", strings.Join(pathScoped, ", "))
	}
	if app == nil {
		note(Warn, "no Cloudflare Access application covers this hostname (it was reachable without a login); set access by hand")
		return m
	}
	m.App = app.Name
	p, ok := mapPolicies(app.Policies, groups, note)
	if !ok {
		note(Warn, "access not imported; set it by hand")
		return m
	}
	if p.empty() {
		note(Warn, "no rule Codigate can check is left; set access by hand")
		return m
	}
	m.Policy = p
	return m
}

func accessDomains(a *CFAccessApp) []string {
	if a.Type != "" && a.Type != "self_hosted" && a.Type != "ssh" && a.Type != "vnc" {
		return nil
	}
	ds := append([]string{a.Domain}, a.SelfHostedDomains...)
	for _, d := range a.Destinations {
		if d.Type == "" || d.Type == "public" {
			ds = append(ds, d.URI)
		}
	}
	out := ds[:0]
	for _, d := range ds {
		if d = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(d), "https://"), "http://"); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// wildcardMatch: "*.example.com" matches "a.example.com", not "a.b.example.com".
func wildcardMatch(pattern, host string) bool {
	suffix, ok := strings.CutPrefix(pattern, "*.")
	if !ok {
		return false
	}
	label, rest, found := strings.Cut(host, ".")
	return found && label != "" && rest == suffix
}

// mapPolicies unions the allow policies' include rules. ok is false when the app has a policy
// that Codigate can't express without letting more people in.
func mapPolicies(policies []CFAccessPolicy, groups GroupLookup, note func(string, string, ...any)) (*Policy, bool) {
	p := &Policy{Emails: []string{}, EmailDomains: []string{}, Groups: []string{}}
	ok := true
	for _, pol := range policies {
		switch pol.Decision {
		case "allow":
			if len(pol.Require) > 0 || len(pol.Exclude) > 0 {
				note(Warn, "policy %q has require or exclude rules; Codigate rules only allow, so it can't be imported safely", pol.Name)
				ok = false
				continue
			}
			if !addRules(p, pol.Include, groups, note, 0) {
				ok = false
			}
		case "deny":
			note(Warn, "deny policy %q has no Codigate equivalent", pol.Name)
			ok = false
		case "bypass":
			note(Warn, "bypass policy %q lets people in without a login; make the app public or use bypass paths in Codigate", pol.Name)
			ok = false
		case "non_identity":
			note(Info, "policy %q is for service tokens or other non-identity checks; create service tokens in Codigate and attach them to the app", pol.Name)
		default:
			note(Warn, "policy %q has unknown decision %q", pol.Name, pol.Decision)
			ok = false
		}
	}
	sort.Strings(p.Emails)
	sort.Strings(p.EmailDomains)
	sort.Strings(p.Groups)
	p.Emails, p.EmailDomains, p.Groups = dedupe(p.Emails), dedupe(p.EmailDomains), dedupe(p.Groups)
	return p, ok
}

// addRules adds include rules (any of which lets a person in). It returns false when a group
// could not be read or has rules that would make the import looser.
func addRules(p *Policy, rules []json.RawMessage, groups GroupLookup, note func(string, string, ...any), depth int) bool {
	ok := true
	for _, raw := range rules {
		var rule map[string]map[string]any
		if json.Unmarshal(raw, &rule) != nil || len(rule) != 1 {
			note(Warn, "unreadable rule %s dropped", truncate(string(raw), 80))
			continue
		}
		for kind, v := range rule {
			str := func(k string) string { s, _ := v[k].(string); return strings.TrimSpace(s) }
			switch kind {
			case "everyone":
				p.Everyone = true
			case "email":
				if e := strings.ToLower(str("email")); strings.Contains(e, "@") {
					p.Emails = append(p.Emails, e)
				}
			case "email_domain":
				if d := strings.ToLower(strings.TrimPrefix(str("domain"), "@")); d != "" {
					p.EmailDomains = append(p.EmailDomains, d)
				}
			case "azureAD":
				if id := str("id"); id != "" {
					p.Groups = append(p.Groups, id)
					note(Info, "Entra ID group %s imported as a group; the provider must send group ids in the groups claim", id)
				}
			case "okta":
				if n := str("name"); n != "" {
					p.Groups = append(p.Groups, n)
				}
			case "saml":
				if g, ok := groupClaim(str("attribute_name"), str("attribute_value")); ok {
					p.Groups = append(p.Groups, g)
				} else {
					note(Warn, "SAML attribute %q dropped; only group attributes map to Codigate groups", str("attribute_name"))
				}
			case "oidc":
				if g, ok := groupClaim(str("claim_name"), str("claim_value")); ok {
					p.Groups = append(p.Groups, g)
				} else {
					note(Warn, "OIDC claim %q dropped; only the groups claim maps to Codigate groups", str("claim_name"))
				}
			case "group":
				if !addGroup(p, str("id"), groups, note, depth) {
					ok = false
				}
			case "any_valid_service_token", "service_token":
				note(Info, "service token rule dropped; create service tokens in Codigate and attach them to the app")
			default:
				note(Warn, "%s rule dropped; Codigate can't check it (fewer people get in, not more)", kind)
			}
		}
	}
	return ok
}

func addGroup(p *Policy, id string, groups GroupLookup, note func(string, string, ...any), depth int) bool {
	if groups == nil || depth >= 3 {
		note(Warn, "Access group %s could not be read", id)
		return false
	}
	g, err := groups(id)
	if err != nil {
		note(Warn, "Access group %s: %v", id, err)
		return false
	}
	if len(g.Require) > 0 || len(g.Exclude) > 0 {
		note(Warn, "Access group %q has require or exclude rules; Codigate rules only allow, so it can't be imported safely", g.Name)
		return false
	}
	return addRules(p, g.Include, groups, note, depth+1)
}

func groupClaim(name, value string) (string, bool) {
	switch strings.ToLower(name) {
	case "groups", "group", "memberof", "roles":
		return value, value != ""
	}
	return "", false
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
