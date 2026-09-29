package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sshNCBridge 在远程主机上执行 `nc -U <socket>`（可前缀 sudo -n），
// 把每一条到 Docker daemon 的连接桥接为一次独立的 SSH 会话。
// 等价于 dbx docker 分支的 unix-over-nc / unix-over-nc-sudo transport，
// 但 SSH 连接由插件后端直接建立（dbx 插件 Host API 无远程命令能力）。
type sshNCBridge struct {
	client *ssh.Client
	socket string
	sudo   bool
}

func dialSSHNC(ctx context.Context, cfg pluginConfig, password, socketPath string, sudo bool) (*sshNCBridge, error) {
	auth := []ssh.AuthMethod{}
	if password != "" {
		auth = append(auth, ssh.Password(password))
	}
	if cfg.SSHPrivateKeyPath != "" {
		key, err := os.ReadFile(cfg.SSHPrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("Failed to read SSH private key: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			if password != "" {
				if s, err2 := ssh.ParsePrivateKeyWithPassphrase(key, []byte(password)); err2 == nil {
					signer = s
				} else {
					return nil, fmt.Errorf("Failed to parse SSH private key: %w", err)
				}
			} else {
				return nil, fmt.Errorf("Failed to parse SSH private key: %w", err)
			}
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if len(auth) == 0 {
		return nil, errors.New("Unix-over-NC requires an SSH password or private key")
	}

	hostKeyCallback, err := resolveHostKeyCallback(cfg)
	if err != nil {
		return nil, err
	}
	sshCfg := &ssh.ClientConfig{
		User:            cfg.SSHUsername,
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}
	addr := net.JoinHostPort(cfg.SSHHost, fmt.Sprintf("%d", cfg.SSHPort))
	client, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("Failed to connect SSH server %s: %w", addr, err)
	}
	return &sshNCBridge{client: client, socket: socketPath, sudo: sudo}, nil
}

// resolveHostKeyCallback 按配置构造主机密钥回调：显式跳过时接受任意密钥
// （仅受信网络），否则按 known_hosts 校验，并对常见失败给出可操作的提示。
func resolveHostKeyCallback(cfg pluginConfig) (ssh.HostKeyCallback, error) {
	if cfg.SSHSkipHostKeyVerify {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	knownHostsPath := cfg.SSHKnownHostsPath
	if knownHostsPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("Cannot locate SSH known_hosts: %w", err)
		}
		knownHostsPath = home + string(os.PathSeparator) + ".ssh" + string(os.PathSeparator) + "known_hosts"
	}
	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("Cannot load SSH known_hosts %s: %w", knownHostsPath, err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := callback(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
			return fmt.Errorf("SSH host key mismatch for %s: the key recorded in %s does not match the server (the server key may have changed). Remove the stale entry with 'ssh-keygen -R %s' and re-scan with 'ssh-keyscan %s >> ~/.ssh/known_hosts', or enable \"Skip SSH host key verification\" only on trusted networks",
				hostname, knownHostsPath, hostname, hostname)
		}
		return fmt.Errorf("SSH host key for %s is not trusted (see %s). Add it with 'ssh-keyscan %s >> ~/.ssh/known_hosts', or enable \"Skip SSH host key verification\" only on trusted networks",
			hostname, knownHostsPath, hostname)
	}, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func (b *sshNCBridge) command() string {
	quoted := shellQuote(b.socket)
	if b.sudo {
		return "sudo -n -- nc -U " + quoted
	}
	return "nc -U " + quoted
}

// pipeConn 把 SSH 会话的 stdin/stdout 适配成 net.Conn。
type pipeConn struct {
	session *ssh.Session
	stdin   io.WriteCloser
	stdout  io.Reader
	done    chan struct{}
	once    sync.Once
}

func (c *pipeConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *pipeConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }
func (c *pipeConn) Close() error {
	var err error
	c.once.Do(func() {
		_ = c.stdin.Close()
		_ = c.session.Close()
		<-c.done
	})
	return err
}

type fakeAddr string

func (a fakeAddr) Network() string { return "ssh-nc" }
func (a fakeAddr) String() string  { return string(a) }

func (c *pipeConn) LocalAddr() net.Addr              { return fakeAddr("local") }
func (c *pipeConn) RemoteAddr() net.Addr             { return fakeAddr("remote") }
func (c *pipeConn) SetDeadline(time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(time.Time) error { return nil }

func (b *sshNCBridge) dial(_ context.Context) (net.Conn, error) {
	session, err := b.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("Failed to open SSH session: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	session.Stderr = io.Discard
	if err := session.Start(b.command()); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("Failed to start nc on remote host (needs nc -U support): %w", err)
	}
	done := make(chan struct{})
	go func() {
		_ = session.Wait()
		close(done)
	}()
	return &pipeConn{session: session, stdin: stdin, stdout: stdout, done: done}, nil
}

func (b *sshNCBridge) close() {
	_ = b.client.Close()
}
