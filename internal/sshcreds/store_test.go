package sshcreds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const samplePriv = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACBsamplekeydatafortestonlynotrealAAAAAAAAAAAAAtzc2gtZW
QyNTUxOQAAACBsamplekeydatafortestonlynotrealAAAAAEAAAAA
-----END OPENSSH PRIVATE KEY-----
`

const samplePub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISampleKeyDataForTestOnly test@awaki\n"

func TestAddListRemoveAndConfig(t *testing.T) {
	dir := t.TempDir()
	// Point home at temp so ~/.ssh/config write is sandboxed
	t.Setenv("HOME", dir)
	st := New(filepath.Join(dir, "data"))

	id, err := st.Add(AddInput{
		Host:       "github.com-gemechisdev",
		HostName:   "github.com",
		User:       "git",
		PrivateKey: samplePriv,
		PublicKey:  samplePub,
		GitHubUser: "gemechisdev",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id.Host != "github.com-gemechisdev" {
		t.Fatalf("host: %s", id.Host)
	}
	if _, err := os.Stat(id.IdentityFile); err != nil {
		t.Fatal(err)
	}
	// mode 0600
	fi, _ := os.Stat(id.IdentityFile)
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("private key perms too open: %v", fi.Mode())
	}

	list, err := st.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}

	cfg, err := os.ReadFile(st.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	body := string(cfg)
	if !strings.Contains(body, "Host github.com-gemechisdev") {
		t.Fatalf("config missing host:\n%s", body)
	}
	if !strings.Contains(body, "IdentityFile "+id.IdentityFile) {
		t.Fatalf("config missing IdentityFile:\n%s", body)
	}

	// user ~/.ssh/config managed section
	userCfg, err := os.ReadFile(filepath.Join(dir, ".ssh", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(userCfg), managedBegin) {
		t.Fatalf("user config missing managed section:\n%s", userCfg)
	}

	cmd := st.SSHCommand()
	if cmd == "" || !strings.Contains(cmd, "-F ") {
		t.Fatalf("SSHCommand: %q", cmd)
	}

	if err := st.Remove("github.com-gemechisdev"); err != nil {
		t.Fatal(err)
	}
	list, _ = st.List()
	if len(list) != 0 {
		t.Fatalf("expected empty after remove, got %d", len(list))
	}
}

func TestRejectsNonKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	st := New(filepath.Join(dir, "data"))
	_, err := st.Add(AddInput{Host: "x", PrivateKey: "not-a-key"})
	if err == nil {
		t.Fatal("expected error")
	}
}
