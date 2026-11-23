package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func wantErr(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error containing %q, got nil", parts)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not contain %q", err, p)
		}
	}
}

func TestLoadDirectoryExplainsTheDockerMount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "codigate.yml")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	wantErr(t, err, dir+" is a directory, not a file", "Docker creates an empty directory", "--force-recreate")
}

func TestLoadMissingFileNamesTheSettings(t *testing.T) {
	t.Setenv("CODIGATE_DOCKER", "")
	_, err := Load(filepath.Join(t.TempDir(), "codigate.yml"))
	wantErr(t, err, "not found", "CODIGATE_CONFIG", "/etc/codigate/codigate.yml")
}

func TestLoadNamesTheFileOnParseErrors(t *testing.T) {
	t.Setenv("CODIGATE_TOKEN", "")
	path := filepath.Join(t.TempDir(), "codigate.yml")
	if err := os.WriteFile(path, []byte("edge: https://connect.codigate.nl\ningress:\n  - service: http_status:404\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	wantErr(t, err, path+": ", "no agent token", "CODIGATE_TOKEN", "token_file")
}

func TestEmptyConfig(t *testing.T) {
	_, err := Parse([]byte(" \n"), env(nil))
	wantErr(t, err, "the config file is empty", "CODIGATE_DOCKER")

	c, err := Parse([]byte(""), env(map[string]string{"CODIGATE_DOCKER": "true", "CODIGATE_EDGE": "https://e.test", "CODIGATE_TOKEN": "t"}))
	if err != nil || !c.Docker.Enabled {
		t.Fatalf("an empty file with CODIGATE_DOCKER is a Docker-only config: %v", err)
	}
}

func TestYAMLErrorsShowTheLine(t *testing.T) {
	_, err := Parse([]byte("edge: https://e.test\ntoken: t\ningress:\n  - hostname: a.example.com\n   service: http://x\n"), env(nil))
	wantErr(t, err, "not valid YAML: line ", "indentation")
	if strings.Contains(err.Error(), "yaml: ") {
		t.Errorf("error keeps the yaml: prefix: %v", err)
	}

	_, err = Parse([]byte("edge: https://e.test\ntoken: t\nigress: []\n"), env(nil))
	wantErr(t, err, "unknown or wrong setting: line 3", "igress")
}

func TestNoIngressRulesSaysWhatToAdd(t *testing.T) {
	_, err := Parse([]byte("edge: https://e.test\ntoken: t\n"), env(nil))
	wantErr(t, err, "no ingress rules", "http_status:404", "CODIGATE_DOCKER=true")
}

func TestCatchAllMustBeLast(t *testing.T) {
	_, err := Parse([]byte("edge: https://e.test\ntoken: t\ningress:\n  - service: http_status:404\n  - hostname: a.example.com\n    service: http://x\n"), env(nil))
	wantErr(t, err, "ingress[0]: a catch-all rule (no hostname) must be the last rule")
}

func TestUnreadableTokenFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "token")
	_, err := Parse([]byte("edge: https://e.test\ntoken_file: "+missing+"\ningress:\n  - service: http_status:404\n"), env(nil))
	wantErr(t, err, "token_file "+missing+" can't be read", "CODIGATE_TOKEN")
}
