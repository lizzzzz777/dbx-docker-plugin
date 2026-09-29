package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/client"
)

// dockerClient 包装一条到 Docker Engine 的连接及其配置。
type dockerClient struct {
	cli    *client.Client
	conn   connectionPayload
	cfg    pluginConfig
	closer func() // 关闭 nc-over-ssh 等拨号器持有的资源
}

func (d *dockerClient) close() {
	if d.closer != nil {
		d.closer()
	}
	if d.cli != nil {
		_ = d.cli.Close()
	}
}

func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(h, "localhost") || h == "" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// hasEnabledTunnel 判断 dbx 宿主是否为该连接建立了传输层隧道；
// 有隧道时 runtime 字段携带本地回环端点。
func hasEnabledTunnel(c connectionPayload) bool {
	for _, layer := range c.TransportLayers {
		t, _ := layer["type"].(string)
		enabled, ok := layer["enabled"].(bool)
		if t != "" && (!ok || enabled) {
			return true
		}
	}
	return false
}

// localSocketDefault 返回本机 Docker 守护进程的默认入口：
// Windows 为命名管道，macOS / Linux 为 /var/run/docker.sock。
func localSocketDefault() string {
	if runtime.GOOS == "windows" {
		return "//./pipe/docker_engine"
	}
	return "/var/run/docker.sock"
}

// isWindowsPipePath 判断路径是否为 Windows 命名管道
//（//./pipe/... 或 \\.\pipe\... 两种书写形式）。
func isWindowsPipePath(p string) bool {
	return strings.HasPrefix(strings.ReplaceAll(p, "\\", "/"), "//./pipe/")
}

// windowsPipeURLPath 把命名管道路径统一成 npipe URL 需要的 //./pipe/... 形式。
func windowsPipeURLPath(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}

// validate 复刻分支 config.rs 的安全约束。
func validateConfig(c connectionPayload, cfg pluginConfig, runtime runtimeEndpoint) error {
	switch cfg.Protocol {
	case "http", "https":
		if strings.TrimSpace(c.Host) == "" && runtime.Port == 0 {
			return errors.New("Docker HTTP/HTTPS connections require a host")
		}
		if cfg.Protocol == "http" && !isLoopbackHost(c.Host) && !hasEnabledTunnel(c) && !cfg.AllowInsecureRemoteHTTP {
			return errors.New("Remote Docker HTTP is disabled. Enable insecure remote HTTP explicitly, or use HTTPS or an SSH tunnel.")
		}
	case "unix", "unix-over-nc", "unix-over-nc-sudo":
		if p := cfg.SocketPath; p != "" {
			if strings.ContainsAny(p, "\x00\n\r") || (!isWindowsPipePath(p) && !strings.HasPrefix(p, "/")) {
				return fmt.Errorf("Invalid Docker socket path: %q", p)
			}
		}
		if cfg.Protocol == "unix" {
			// 本机直连：socket 留空时按操作系统自动选择默认入口。
		} else {
			if strings.TrimSpace(cfg.SSHHost) == "" {
				return errors.New("Unix-over-NC requires an SSH host (see the connection form)")
			}
		}
	default:
		return fmt.Errorf("Unsupported Docker protocol: %s", cfg.Protocol)
	}
	if cfg.APIVersion != "" && cfg.APIVersion != "auto" {
		v := strings.TrimPrefix(cfg.APIVersion, "v")
		var major, minor int
		if _, err := fmt.Sscanf(v, "%d.%d", &major, &minor); err != nil || major <= 0 {
			return fmt.Errorf("Invalid Docker API version: %q", cfg.APIVersion)
		}
		if major == 1 && minor < 24 {
			return errors.New("Docker API versions older than 1.24 are not supported")
		}
	}
	return nil
}

func buildTLSConfig(cfg pluginConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CACertPath != "" {
		pem, err := os.ReadFile(cfg.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("Failed to read Docker CA certificate: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("Docker CA certificate is not a valid PEM file")
		}
		tlsCfg.RootCAs = pool
	}
	if (cfg.ClientCertPath == "") != (cfg.ClientKeyPath == "") {
		return nil, errors.New("Docker HTTPS requires both a client certificate and private key")
	}
	if cfg.ClientCertPath != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCertPath, cfg.ClientKeyPath)
		if err != nil {
			return nil, fmt.Errorf("Failed to load Docker client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return tlsCfg, nil
}

// connect 按协议构造 docker client 并完成版本协商 + ping。
func connect(ctx context.Context, c connectionPayload, runtime runtimeEndpoint) (*dockerClient, error) {
	cfg := c.config()
	if err := validateConfig(c, cfg, runtime); err != nil {
		return nil, err
	}

	opts := []client.Opt{}
	var closer func()

	// 自定义拨号集合：隧道与 nc 桥都会改写拨号目标，最终用统一的
	// WithHTTPClient(Transport{DialContext}) 组装。官方 SDK 的 Dialer()（hijack
	// 流：exec attach、日志 follow 都用它）在 baseTransport.DialContext 为空时
	// 退化为 net.Dial(cli.proto,…)，而 http/https 协议的 proto 是 "http"/"https"，
	// 直接报 "dial http: unknown network http"。因此所有 TCP 类协议都必须显式
	// 提供拨号器，不能只依赖 WithHost 的默认传输配置。
	var dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	var tlsCfg *tls.Config

	switch cfg.Protocol {
	case "http", "https":
		host := strings.TrimSpace(c.Host)
		port := c.Port
		// dbx 隧道激活时拨本地回环端点，HTTP Host/TLS SNI 保持原始主机。
		if hasEnabledTunnel(c) && runtime.Host != "" && runtime.Port > 0 {
			dialTarget := net.JoinHostPort(runtime.Host, fmt.Sprintf("%d", runtime.Port))
			dialer := &net.Dialer{Timeout: 15 * time.Second}
			dialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp", dialTarget)
			}
		}
		if port == 0 {
			if cfg.Protocol == "https" {
				port = 2376
			} else {
				port = 2375
			}
		}
		scheme := cfg.Protocol
		hostURL := fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(host, fmt.Sprintf("%d", port)))
		opts = append(opts, client.WithHost(hostURL))
		if scheme == "https" {
			cfgTLS, err := buildTLSConfig(cfg)
			if err != nil {
				return nil, err
			}
			tlsCfg = cfgTLS
		}
		if dialContext == nil {
			dialer := &net.Dialer{Timeout: 15 * time.Second}
			dialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp", addr)
			}
		}

	case "unix":
		socketPath := strings.TrimSpace(cfg.SocketPath)
		if socketPath == "" {
			socketPath = localSocketDefault()
		}
		if isWindowsPipePath(socketPath) {
			// docker SDK 的 npipe transport 直连 Windows 命名管道。
			opts = append(opts, client.WithHost("npipe://"+windowsPipeURLPath(socketPath)))
		} else {
			opts = append(opts, client.WithHost("unix://"+socketPath))
		}

	case "unix-over-nc", "unix-over-nc-sudo":
		socketPath := cfg.SocketPath
		if strings.TrimSpace(socketPath) == "" {
			socketPath = "/var/run/docker.sock"
		}
		bridge, err := dialSSHNC(ctx, cfg, c.secret("ssh_password"), socketPath, cfg.Protocol == "unix-over-nc-sudo")
		if err != nil {
			return nil, err
		}
		closer = bridge.close
		dialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return bridge.dial(ctx)
		}
		opts = append(opts, client.WithHost("http://docker-over-ssh"))
	}

	if dialContext != nil || tlsCfg != nil {
		transport := &http.Transport{TLSClientConfig: tlsCfg, DialContext: dialContext}
		opts = append(opts, client.WithHTTPClient(&http.Client{Transport: transport}))
	}

	if cfg.APIVersion != "" && cfg.APIVersion != "auto" {
		opts = append(opts, client.WithVersion(strings.TrimPrefix(cfg.APIVersion, "v")))
	} else {
		opts = append(opts, client.WithAPIVersionNegotiation())
	}

	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		if closer != nil {
			closer()
		}
		return nil, err
	}

	dc := &dockerClient{cli: cli, conn: c, cfg: cfg, closer: closer}

	// 分支行为：连接建立即 ping 验证。
	pingCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := cli.Ping(pingCtx); err != nil {
		dc.close()
		return nil, fmt.Errorf("Failed to reach Docker daemon: %w", err)
	}
	return dc, nil
}
