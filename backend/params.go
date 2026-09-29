package main

import (
	"encoding/json"
	"errors"

	sdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// 所有 RPC 方法共用的入参信封。连接生命周期方法（connection/test 等）带
// connection + runtime；业务方法（docker/*）只带 connectionId 与会话路由。
type params struct {
	Connection   connectionPayload `json:"connection"`
	Runtime      runtimeEndpoint   `json:"runtime"`
	ConnectionID string            `json:"connectionId"`
}

type runtimeEndpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// connectionPayload 是宿主下发的完整 ConnectionConfig 中本插件关心的部分。
type connectionPayload struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Host            string            `json:"host"`
	Port            int               `json:"port"`
	ReadOnly        bool              `json:"read_only"`
	Color           string            `json:"color"`
	ExternalConfig  map[string]any    `json:"external_config"`
	Secrets         map[string]string `json:"connection_secrets"`
	TransportLayers []map[string]any  `json:"transport_layers"`
}

// pluginConfig 对应 manifest 里 binding:"config" 的字段集合。
type pluginConfig struct {
	Protocol                string `json:"protocol"`
	SocketPath              string `json:"socket_path"`
	APIVersion              string `json:"api_version"`
	AllowInsecureRemoteHTTP bool   `json:"allow_insecure_remote_http"`
	CACertPath              string `json:"ca_cert_path"`
	ClientCertPath          string `json:"client_cert_path"`
	ClientKeyPath           string `json:"client_key_path"`
	SSHHost                 string `json:"ssh_host"`
	SSHPort                 int    `json:"ssh_port"`
	SSHUsername             string `json:"ssh_username"`
	SSHPrivateKeyPath       string `json:"ssh_private_key_path"`
	SSHKnownHostsPath       string `json:"ssh_known_hosts_path"`
	SSHSkipHostKeyVerify    bool   `json:"ssh_skip_host_key_verify"`
	ReadOnly                bool   `json:"read_only"`
	IsProduction            bool   `json:"is_production"`
}

// SocketPath 留空表示自动：本机 unix 模式按操作系统取默认入口
//（Windows 命名管道，其余 /var/run/docker.sock），SSH 模式取对端默认。
func (c connectionPayload) config() pluginConfig {
	cfg := pluginConfig{
		Protocol:   "http",
		APIVersion: "auto",
		SSHPort:    22,
	}
	raw := c.ExternalConfig
	if raw == nil {
		return cfg
	}
	get := func(key string, dst *string) {
		if v, ok := raw[key].(string); ok && v != "" {
			*dst = v
		}
	}
	get("protocol", &cfg.Protocol)
	get("socket_path", &cfg.SocketPath)
	get("api_version", &cfg.APIVersion)
	get("ca_cert_path", &cfg.CACertPath)
	get("client_cert_path", &cfg.ClientCertPath)
	get("client_key_path", &cfg.ClientKeyPath)
	get("ssh_host", &cfg.SSHHost)
	get("ssh_username", &cfg.SSHUsername)
	get("ssh_private_key_path", &cfg.SSHPrivateKeyPath)
	get("ssh_known_hosts_path", &cfg.SSHKnownHostsPath)
	if v, ok := raw["ssh_port"].(float64); ok && v > 0 {
		cfg.SSHPort = int(v)
	}
	if v, ok := raw["allow_insecure_remote_http"].(bool); ok {
		cfg.AllowInsecureRemoteHTTP = v
	}
	if v, ok := raw["ssh_skip_host_key_verify"].(bool); ok {
		cfg.SSHSkipHostKeyVerify = v
	}
	if v, ok := raw["read_only"].(bool); ok {
		cfg.ReadOnly = v
	}
	if v, ok := raw["is_production"].(bool); ok {
		cfg.IsProduction = v
	}
	return cfg
}

// effectiveReadOnly 与 xxl-job 先例一致：内建 read_only 与插件自声明字段取或。
func (c connectionPayload) effectiveReadOnly() bool {
	return c.ReadOnly || c.config().ReadOnly
}

func (c connectionPayload) secret(key string) string {
	if c.Secrets == nil {
		return ""
	}
	return c.Secrets[key]
}

func invalidParams() *sdk.PluginError {
	return sdk.NewError(-32602, "Invalid request parameters")
}

// errInvalidParams 供内部 dispatch 链使用（PluginError 未实现 error 接口）。
var errInvalidParams = errors.New("invalid request parameters")

func serverError(err error) *sdk.PluginError {
	return sdk.NewError(-32000, err.Error())
}

// decodeParams 解析公共信封；业务方法只需要 connectionId。
func decodeParams(raw json.RawMessage) (params, *sdk.PluginError) {
	var p params
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, invalidParams()
	}
	return p, nil
}

var errMethodNotFound = errors.New("method not found")
