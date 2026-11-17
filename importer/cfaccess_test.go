package importer

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const accessApps = `[
 {"name":"Grafana","type":"self_hosted","domain":"grafana.example.com","policies":[
   {"name":"staff","decision":"allow","include":[{"email_domain":{"domain":"example.com"}},{"email":{"email":"Ext@Partner.org"}}]},
   {"name":"ops","decision":"allow","include":[{"group":{"id":"11111111-1111-1111-1111-111111111111"}},{"ip":{"ip":"10.0.0.0/8"}}]},
   {"name":"ci","decision":"non_identity","include":[{"any_valid_service_token":{}}]}]},
 {"name":"Wildcard","type":"self_hosted","domain":"*.example.com","policies":[
   {"name":"all","decision":"allow","include":[{"everyone":{}}]}]},
 {"name":"NAS","type":"self_hosted","domain":"nas.example.com","policies":[
   {"name":"admins","decision":"allow","include":[{"azureAD":{"id":"aad-group-1","identity_provider_id":"x"}}],"require":[{"country":{"country_code":"NL"}}]}]},
 {"name":"NAS API","type":"self_hosted","domain":"nas.example.com/api","policies":[{"name":"open","decision":"bypass","include":[{"everyone":{}}]}]},
 {"name":"Wiki","type":"self_hosted","destinations":[{"type":"public","uri":"wiki.example.com"}],"policies":[
   {"name":"blocked","decision":"deny","include":[{"email":{"email":"x@example.com"}}]},
   {"name":"all","decision":"allow","include":[{"everyone":{}}]}]},
 {"name":"Docs","type":"self_hosted","domain":"docs.example.com","policies":[
   {"name":"geo","decision":"allow","include":[{"geo":{"country_code":"NL"}}]}]},
 {"name":"Team","type":"self_hosted","self_hosted_domains":["team.example.com"],"policies":[
   {"name":"g","decision":"allow","include":[{"group":{"id":"22222222-2222-2222-2222-222222222222"}}]}]},
 {"name":"SaaS","type":"saas","domain":"saas.example.org","policies":[{"name":"all","decision":"allow","include":[{"everyone":{}}]}]}
]`

func groupsFake(id string) (*CFAccessPolicy, error) {
	switch id {
	case "11111111-1111-1111-1111-111111111111":
		var g CFAccessPolicy
		json.Unmarshal([]byte(`{"name":"Ops","include":[{"okta":{"name":"ops-team"}},{"saml":{"attribute_name":"groups","attribute_value":"sre"}},{"oidc":{"claim_name":"department","claim_value":"x"}}]}`), &g)
		return &g, nil
	case "22222222-2222-2222-2222-222222222222":
		var g CFAccessPolicy
		json.Unmarshal([]byte(`{"name":"Team","include":[{"email":{"email":"a@example.com"}}],"exclude":[{"email":{"email":"b@example.com"}}]}`), &g)
		return &g, nil
	}
	return nil, errors.New("not found")
}

func TestMapAccess(t *testing.T) {
	var apps []CFAccessApp
	if err := json.Unmarshal([]byte(accessApps), &apps); err != nil {
		t.Fatal(err)
	}
	hosts := []string{"grafana.example.com", "nas.example.com", "wiki.example.com", "docs.example.com", "team.example.com", "photos.example.com", "a.b.example.com", "saas.example.org"}
	got := map[string]AccessMapping{}
	for _, m := range MapAccess(apps, hosts, groupsFake) {
		got[m.Hostname] = m
	}
	notes := func(h string) string {
		var s []string
		for _, n := range got[h].Notes {
			s = append(s, n.String())
		}
		return strings.Join(s, "\n")
	}

	// Union of the allow policies; the IP rule and the department claim are dropped (stricter).
	g := got["grafana.example.com"]
	want := &Policy{Emails: []string{"ext@partner.org"}, EmailDomains: []string{"example.com"}, Groups: []string{"ops-team", "sre"}}
	if g.App != "Grafana" || !reflect.DeepEqual(g.Policy, want) {
		t.Errorf("grafana: %q %+v\n%s", g.App, g.Policy, notes("grafana.example.com"))
	}
	for _, s := range []string{"ip rule dropped", `OIDC claim "department" dropped`, "service tokens"} {
		if !strings.Contains(notes("grafana.example.com"), s) {
			t.Errorf("grafana notes lack %q:\n%s", s, notes("grafana.example.com"))
		}
	}

	// Anything that would let more people in leaves the app alone.
	for host, why := range map[string]string{
		"nas.example.com":  "require or exclude",
		"wiki.example.com": "deny policy",
		"team.example.com": `group "Team" has require or exclude`,
		"docs.example.com": "no rule Codigate can check",
		"a.b.example.com":  "no Cloudflare Access application covers",
		"saas.example.org": "no Cloudflare Access application covers", // a SaaS app is not a hostname
	} {
		if got[host].Policy != nil || !strings.Contains(notes(host), why) {
			t.Errorf("%s: policy %+v, notes:\n%s", host, got[host].Policy, notes(host))
		}
	}
	if !strings.Contains(notes("nas.example.com"), "NAS API (/api)") {
		t.Errorf("path-scoped app not reported:\n%s", notes("nas.example.com"))
	}

	// The wildcard covers one label.
	if p := got["photos.example.com"]; p.App != "Wildcard" || p.Policy == nil || !p.Policy.Everyone {
		t.Errorf("photos: %+v", p)
	}
}

func TestMapAccessWithoutGroupLookup(t *testing.T) {
	apps := []CFAccessApp{{Name: "X", Domain: "x.example.com", Policies: []CFAccessPolicy{{Name: "p", Decision: "allow",
		Include: []json.RawMessage{json.RawMessage(`{"group":{"id":"33333333-3333-3333-3333-333333333333"}}`), json.RawMessage(`{"email":{"email":"a@example.com"}}`)}}}}}
	if m := MapAccess(apps, []string{"x.example.com"}, nil)[0]; m.Policy != nil {
		t.Fatalf("unreadable group still imported: %+v", m.Policy)
	}
}
