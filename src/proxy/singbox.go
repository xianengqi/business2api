package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"

	// 启用 QUIC 协议支持（hysteria, hysteria2, tuic）
	_ "github.com/sagernet/sing-quic/hysteria"
	_ "github.com/sagernet/sing-quic/hysteria2"
	_ "github.com/sagernet/sing-quic/tuic"
)

type SingboxManager struct {
	mu        sync.Mutex
	instances map[int]*SingboxInstance
	basePort  int
	ready     bool
}

// SingboxInstance sing-box 实例
type SingboxInstance struct {
	Port     int
	Box      *box.Box
	Ctx      context.Context
	Cancel   context.CancelFunc
	Running  bool
	ProxyURL string
	Node     *ProxyNode
}

var singboxMgr = &SingboxManager{
	instances: make(map[int]*SingboxInstance),
	basePort:  11800,
	ready:     true,
}

// IsSingboxProtocol 所有协议都由 sing-box 处理
func IsSingboxProtocol(protocol string) bool {
	switch protocol {
	case "vmess", "vless", "shadowsocks", "trojan", "socks", "http",
		"hysteria", "hysteria2", "hy2", "tuic", "wireguard", "anytls":
		return true
	}
	return false
}

func CanSingboxHandle(protocol string) bool {
	return IsSingboxProtocol(protocol)
}

func (sm *SingboxManager) IsAvailable() bool {
	return sm.ready
}

func (sm *SingboxManager) Start(node *ProxyNode) (string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 分配端口
	port := sm.findAvailablePort()
	if port == 0 {
		return "", fmt.Errorf("无可用端口")
	}
	configJSON := sm.generateConfigJSON(node, port)
	ctx, cancel := context.WithCancel(context.Background())
	ctx = box.Context(ctx, include.InboundRegistry(), include.OutboundRegistry(),
		include.EndpointRegistry(), include.DNSTransportRegistry(), include.ServiceRegistry())
	var opts option.Options
	err := opts.UnmarshalJSONContext(ctx, []byte(configJSON))
	if err != nil {
		cancel()
		return "", fmt.Errorf("解析配置失败: %w", err)
	}

	singBox, err := box.New(box.Options{
		Context: ctx,
		Options: opts,
	})
	if err != nil {
		cancel()
		return "", fmt.Errorf("创建 sing-box 失败: %w", err)
	}

	if err := singBox.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("启动 sing-box 失败: %w", err)
	}

	// 等待端口就绪
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	portReady := false
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			portReady = true
			break
		}
	}
	if !portReady {
		singBox.Close()
		cancel()
		return "", fmt.Errorf("端口 %d 未就绪", port)
	}
	proxyURLParsed, _ := url.Parse(proxyURL)
	testClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURLParsed),
		},
		Timeout: 5 * time.Second,
	}
	testResp, testErr := testClient.Get("https://www.gstatic.com/generate_204")
	if testErr != nil {
		singBox.Close()
		cancel()
		return "", fmt.Errorf("连通性测试失败: %w", testErr)
	}
	testResp.Body.Close()
	if testResp.StatusCode != 204 && testResp.StatusCode != 200 {
		singBox.Close()
		cancel()
		return "", fmt.Errorf("连通性测试状态码: %d", testResp.StatusCode)
	}

	instance := &SingboxInstance{
		Port:     port,
		Box:      singBox,
		Ctx:      ctx,
		Cancel:   cancel,
		Running:  true,
		ProxyURL: proxyURL,
		Node:     node,
	}

	sm.instances[port] = instance
	node.LocalPort = port
	return proxyURL, nil
}

// Stop 停止实例
func (sm *SingboxManager) Stop(port int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if inst, ok := sm.instances[port]; ok {
		if inst.Box != nil {
			inst.Box.Close()
		}
		if inst.Cancel != nil {
			inst.Cancel()
		}
		inst.Running = false
		delete(sm.instances, port)
	}
}

// StopAll 停止所有实例
func (sm *SingboxManager) StopAll() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for port, inst := range sm.instances {
		if inst.Box != nil {
			inst.Box.Close()
		}
		if inst.Cancel != nil {
			inst.Cancel()
		}
		delete(sm.instances, port)
	}
}

func (sm *SingboxManager) findAvailablePort() int {
	for port := sm.basePort; port < sm.basePort+1000; port++ {
		if _, exists := sm.instances[port]; !exists {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
			if err != nil {
				return port
			}
			conn.Close()
		}
	}
	return 0
}

// generateConfigJSON 生成 sing-box JSON 配置
func (sm *SingboxManager) generateConfigJSON(node *ProxyNode, localPort int) string {
	outbound := sm.buildOutboundJSON(node)

	config := map[string]interface{}{
		"log": map[string]interface{}{
			"disabled": true,
		},
		"inbounds": []map[string]interface{}{
			{
				"type":        "http",
				"tag":         "http-in",
				"listen":      "127.0.0.1",
				"listen_port": localPort,
			},
		},
		"outbounds": []interface{}{
			outbound,
		},
	}

	data, _ := json.Marshal(config)
	return string(data)
}

// buildOutboundJSON 构建 outbound JSON 配置
func (sm *SingboxManager) buildOutboundJSON(node *ProxyNode) map[string]interface{} {
	sni := node.SNI
	if sni == "" {
		sni = node.Server
	}

	switch node.Protocol {
	case "hysteria2", "hy2":
		return map[string]interface{}{
			"type":        "hysteria2",
			"tag":         "proxy",
			"server":      node.Server,
			"server_port": node.Port,
			"password":    node.Password,
			"tls": map[string]interface{}{
				"enabled":     true,
				"insecure":    true,
				"server_name": sni,
			},
		}

	case "hysteria":
		return map[string]interface{}{
			"type":        "hysteria",
			"tag":         "proxy",
			"server":      node.Server,
			"server_port": node.Port,
			"auth_str":    node.Password,
			"tls": map[string]interface{}{
				"enabled":     true,
				"insecure":    true,
				"server_name": sni,
			},
		}

	case "tuic":
		return map[string]interface{}{
			"type":        "tuic",
			"tag":         "proxy",
			"server":      node.Server,
			"server_port": node.Port,
			"uuid":        node.UUID,
			"password":    node.Password,
			"tls": map[string]interface{}{
				"enabled":     true,
				"insecure":    true,
				"server_name": sni,
			},
		}

	case "vmess":
		out := map[string]interface{}{
			"type":        "vmess",
			"tag":         "proxy",
			"server":      node.Server,
			"server_port": node.Port,
			"uuid":        node.UUID,
			"security":    node.Security,
			"alter_id":    node.AlterId,
		}
		if node.TLS {
			out["tls"] = map[string]interface{}{
				"enabled":     true,
				"insecure":    true,
				"server_name": sni,
			}
		}
		if transport := sm.buildTransportJSON(node); transport != nil {
			out["transport"] = transport
		}
		return out

	case "vless":
		out := map[string]interface{}{
			"type":        "vless",
			"tag":         "proxy",
			"server":      node.Server,
			"server_port": node.Port,
			"uuid":        node.UUID,
		}
		if node.Flow != "" {
			out["flow"] = node.Flow
		}
		if node.Security == "reality" {
			fp := node.Fingerprint
			if fp == "" {
				fp = "chrome"
			}
			out["tls"] = map[string]interface{}{
				"enabled":     true,
				"server_name": node.SNI,
				"reality": map[string]interface{}{
					"enabled":    true,
					"public_key": node.PublicKey,
					"short_id":   node.ShortId,
				},
				"utls": map[string]interface{}{
					"enabled":     true,
					"fingerprint": fp,
				},
			}
		} else if node.TLS {
			out["tls"] = map[string]interface{}{
				"enabled":     true,
				"insecure":    true,
				"server_name": sni,
			}
		}
		if transport := sm.buildTransportJSON(node); transport != nil {
			out["transport"] = transport
		}
		return out

	case "shadowsocks":
		return map[string]interface{}{
			"type":        "shadowsocks",
			"tag":         "proxy",
			"server":      node.Server,
			"server_port": node.Port,
			"method":      node.Method,
			"password":    node.Password,
		}

	case "trojan":
		out := map[string]interface{}{
			"type":        "trojan",
			"tag":         "proxy",
			"server":      node.Server,
			"server_port": node.Port,
			"password":    node.Password,
			"tls": map[string]interface{}{
				"enabled":     true,
				"insecure":    true,
				"server_name": sni,
			},
		}
		if transport := sm.buildTransportJSON(node); transport != nil {
			out["transport"] = transport
		}
		return out

	default:
		return map[string]interface{}{
			"type": "direct",
			"tag":  "proxy",
		}
	}
}

// buildTransportJSON 构建传输层 JSON 配置
func (sm *SingboxManager) buildTransportJSON(node *ProxyNode) map[string]interface{} {
	switch node.Network {
	case "ws":
		transport := map[string]interface{}{
			"type": "ws",
			"path": node.Path,
		}
		if node.Host != "" {
			transport["headers"] = map[string]string{
				"Host": node.Host,
			}
		}
		return transport
	case "grpc":
		return map[string]interface{}{
			"type":         "grpc",
			"service_name": node.Path,
		}
	case "httpupgrade":
		return map[string]interface{}{
			"type": "httpupgrade",
			"path": node.Path,
			"host": node.Host,
		}
	case "h2", "http":
		return map[string]interface{}{
			"type": "http",
			"path": node.Path,
			"host": []string{node.Host},
		}
	}
	return nil
}

// GetSingboxManager 获取 sing-box 管理器
func GetSingboxManager() *SingboxManager {
	return singboxMgr
}

// InitSingbox 初始化 sing-box（内置 core 无需初始化）
func InitSingbox() {

}

// TrySingboxStart 尝试用 sing-box 启动节点（xray 失败时的回退）
func TrySingboxStart(node *ProxyNode) (string, error) {
	if !CanSingboxHandle(node.Protocol) {
		return "", fmt.Errorf("sing-box 不支持协议: %s", node.Protocol)
	}
	return singboxMgr.Start(node)
}

// StopSingbox 停止指定端口的 sing-box 实例
func StopSingbox(port int) {
	singboxMgr.Stop(port)
}

// ParseProxyLinkWithSingbox 使用 sing-box 解析代理链接
func ParseProxyLinkWithSingbox(link string) *ProxyNode {
	node := Manager.parseLine(link)
	if node != nil {
		return node
	}

	link = strings.TrimSpace(link)
	if strings.HasPrefix(link, "hy2://") || strings.HasPrefix(link, "hysteria2://") {
		return parseHysteria2(link)
	}
	if strings.HasPrefix(link, "tuic://") {
		return parseTUIC(link)
	}

	return nil
}

// parseTUIC 解析 TUIC 链接
func parseTUIC(link string) *ProxyNode {
	origLink := link
	link = strings.TrimPrefix(link, "tuic://")

	var name string
	if idx := strings.LastIndex(link, "#"); idx != -1 {
		name, _ = url.QueryUnescape(link[idx+1:])
		link = link[:idx]
	}

	var params string
	if idx := strings.Index(link, "?"); idx != -1 {
		params = link[idx+1:]
		link = link[:idx]
	}

	atIdx := strings.LastIndex(link, "@")
	if atIdx == -1 {
		return nil
	}

	userPart := link[:atIdx]
	hostPart := link[atIdx+1:]

	var uuid, password string
	if colonIdx := strings.Index(userPart, ":"); colonIdx != -1 {
		uuid = userPart[:colonIdx]
		password = userPart[colonIdx+1:]
	} else {
		uuid = userPart
	}

	var server string
	var port int
	if lastColon := strings.LastIndex(hostPart, ":"); lastColon != -1 {
		server = hostPart[:lastColon]
		port, _ = strconv.Atoi(hostPart[lastColon+1:])
	}

	node := &ProxyNode{
		Protocol: "tuic",
		Name:     name,
		Server:   server,
		Port:     port,
		UUID:     uuid,
		Password: password,
		Raw:      origLink,
	}

	for _, param := range strings.Split(params, "&") {
		if kv := strings.SplitN(param, "=", 2); len(kv) == 2 {
			switch kv[0] {
			case "sni":
				node.SNI = kv[1]
			case "alpn":
				node.ALPN = kv[1]
			}
		}
	}

	if node.Server == "" || node.Port == 0 {
		return nil
	}

	return node
}
