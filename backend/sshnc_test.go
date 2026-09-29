package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

func TestResolveHostKeyCallbackSkipVerify(t *testing.T) {
	cb, err := resolveHostKeyCallback(pluginConfig{SSHSkipHostKeyVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("h:22", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}, testPublicKey(t)); err != nil {
		t.Fatalf("skip-verify callback should accept any key, got %v", err)
	}
}

func TestResolveHostKeyCallbackMissingFile(t *testing.T) {
	_, err := resolveHostKeyCallback(pluginConfig{SSHKnownHostsPath: t.TempDir() + "/missing_known_hosts"})
	if err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("err = %v, want known_hosts load failure", err)
	}
}

func TestResolveHostKeyCallbackMismatchMessage(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/known_hosts"
	// 写入同主机名的另一把密钥 → 触发 mismatch 分支。
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	line := "172.18.20.136 " + strings.TrimRight(string(ssh.MarshalAuthorizedKey(signer.PublicKey())), "\n") + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	cb, err := resolveHostKeyCallback(pluginConfig{SSHKnownHostsPath: path})
	if err != nil {
		t.Fatal(err)
	}
	err = cb("172.18.20.136:22", &net.TCPAddr{IP: net.ParseIP("172.18.20.136"), Port: 22}, testPublicKey(t))
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	if !strings.Contains(err.Error(), "key mismatch") || !strings.Contains(err.Error(), "ssh-keygen -R") {
		t.Fatalf("mismatch guidance missing: %v", err)
	}
}
