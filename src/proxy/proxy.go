package proxy

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tlsConfig 全局 TLS 配置，跳过证书验证
var tlsConfig = &tls.Config{InsecureSkipVerify: true}

// ProxyNode 代理节点
type ProxyNode struct {
	Raw       string // 原始链接
	Protocol  string // vmess, vless, ss, trojan, http, socks5, hysteria2, anytls
	Name      string
	Server    string
	Port      int
	UUID      string // vmess/vless
	AlterId   int    // vmess
	Security  string // vmess 加密方式 / vless: none,tls,reality
	Network   string // tcp, ws, grpc, kcp, quic, httpupgrade, splithttp, xhttp
	Path      string // ws/http path
	Host      string // ws/http host
	TLS       bool
	SNI       string
	Password  string // ss/trojan/anytls password
	Method    string // ss method
	Type      string // kcp/quic header type (none, srtp, utp, wechat-video, dtls, wireguard)
	Healthy   bool
	LastCheck time.Time
	LocalPort int
	Latency   time.Duration
	ExitIP    string

	// Reality 相关
	Flow        string // xtls-rprx-vision
	Fingerprint string // chrome, firefox, safari, ios, android, edge, 360, qq, random
	PublicKey   string // reality pbk
	ShortId     string // reality sid
	SpiderX     string // reality spx
	ALPN        string

	// 使用统计
	LastUsed    time.Time     // 最后使用时间
	FailCount   int           // 连续失败次数
	UseCooldown time.Duration // 使用冷却时间（失败后动态调整）
}

// InstanceStatus 实例状态
type InstanceStatus int

const (
	InstanceStatusIdle    InstanceStatus = iota // 空闲可用
	InstanceStatusInUse                         // 使用中
	InstanceStatusStopped                       // 已停止
)

// ProxyInstance 代理实例（使用 sing-box）
type ProxyInstance struct {
	localPort int
	node      *ProxyNode
	running   bool
	status    InstanceStatus
	lastUsed  time.Time
	proxyURL  string
	mu        sync.Mutex
}

// ProxyManager 代理管理器
type ProxyManager struct {
	mu             sync.RWMutex
	nodes          []*ProxyNode
	healthyNodes   []*ProxyNode
	instancePool   []*ProxyInstance // 活跃实例追踪
	maxPoolSize    int              // 最大实例池大小
	subscribeURLs  []string
	proxyFiles     []string
	lastUpdate     time.Time
	updateInterval time.Duration
	checkInterval  time.Duration
	healthCheckURL string
	stopChan       chan struct{}
	ready          bool       // 代理池是否就绪
	readyCond      *sync.Cond // 就绪条件变量
	healthChecking bool       // 是否正在健康检查
}

// 默认代理使用冷却时间
var (
	DefaultProxyUseCooldown = 5 * time.Second // 默认使用冷却
	MaxProxyFailCount       = 3               // 最大连续失败次数，超过后增加冷却
	DefaultProxyCount       = 5               // 默认代理池大小
	MinHealthyForReady      = 1               // 最少健康节点数才提示就绪（改为1，更快就绪）
	HealthCheckTimeout      = 8 * time.Second // 健康检查超时
)

var Manager = &ProxyManager{
	instancePool:   make([]*ProxyInstance, 0),
	maxPoolSize:    5,
	updateInterval: 30 * time.Minute,
	checkInterval:  5 * time.Minute,
	healthCheckURL: "https://www.google.com/generate_204",
	stopChan:       make(chan struct{}),
}

func init() {
	Manager.readyCond = sync.NewCond(&Manager.mu)
}

// IsReady 检查代理池是否就绪
func (pm *ProxyManager) IsReady() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.ready
}
func (pm *ProxyManager) WaitReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		pm.mu.RLock()
		ready := pm.ready
		healthyCount := len(pm.healthyNodes)
		pm.mu.RUnlock()

		if ready || healthyCount > 0 {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.ready || len(pm.healthyNodes) > 0
}

// SetReady 设置就绪状态
func (pm *ProxyManager) SetReady(ready bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.ready = ready
	if ready {
		pm.readyCond.Broadcast()
	}
}

// SetMaxPoolSize 设置最大实例池大小
func (pm *ProxyManager) SetMaxPoolSize(size int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if size > 0 {
		pm.maxPoolSize = size
	}
}

// InitInstancePool 初始化实例池（按需启动指定数量的代理实例）
func (pm *ProxyManager) InitInstancePool(count int) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if len(pm.healthyNodes) == 0 && len(pm.nodes) == 0 {
		return fmt.Errorf("没有可用的代理节点")
	}

	if count > pm.maxPoolSize {
		count = pm.maxPoolSize
	}

	nodes := pm.healthyNodes
	if len(nodes) == 0 {
		nodes = pm.nodes
	}

	log.Printf("🔧 初始化代理实例池: 目标 %d 个实例", count)

	for i := 0; i < count && i < len(nodes); i++ {
		node := nodes[i%len(nodes)]
		instance, err := pm.startInstanceLocked(node)
		if err != nil {
			log.Printf("⚠️ 启动实例 %d 失败: %v", i, err)
			continue
		}
		instance.status = InstanceStatusIdle
		pm.instancePool = append(pm.instancePool, instance)
	}

	log.Printf("✅ 实例池初始化完成: %d 个实例就绪", len(pm.instancePool))
	return nil
}

func (pm *ProxyManager) SetXrayPath(path string) {
}

// AddSubscribeURL 添加订阅链接
func (pm *ProxyManager) AddSubscribeURL(url string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.subscribeURLs = append(pm.subscribeURLs, url)
}

// AddProxyFile 添加代理文件
func (pm *ProxyManager) AddProxyFile(path string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.proxyFiles = append(pm.proxyFiles, path)
}

// LoadAll 加载所有代理源
func (pm *ProxyManager) LoadAll() error {
	var allNodes []*ProxyNode

	// 从订阅加载
	for _, url := range pm.subscribeURLs {
		nodes, err := pm.loadFromURL(url)
		if err != nil {
			log.Printf("⚠️ 加载订阅失败 %s: %v", url, err)
			continue
		}
		allNodes = append(allNodes, nodes...)
	}

	// 从文件加载
	for _, file := range pm.proxyFiles {
		nodes, err := pm.loadFromFile(file)
		if err != nil {
			log.Printf("⚠️ 加载文件失败 %s: %v", file, err)
			continue
		}
		allNodes = append(allNodes, nodes...)
	}

	pm.mu.Lock()
	pm.nodes = allNodes
	pm.lastUpdate = time.Now()
	pm.mu.Unlock()

	log.Printf("✅ 共加载 %d 个代理节点", len(allNodes))
	return nil
}

type SubscriptionInfo struct {
	Upload   int64
	Download int64
	Total    int64
	Expire   int64
}

// parseSubscriptionUserinfo 解析 subscription-userinfo 头
func parseSubscriptionUserinfo(header string) *SubscriptionInfo {
	if header == "" {
		return nil
	}
	info := &SubscriptionInfo{}
	parts := strings.Split(header, ";")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		value, _ := strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
		switch key {
		case "upload":
			info.Upload = value
		case "download":
			info.Download = value
		case "total":
			info.Total = value
		case "expire":
			info.Expire = value
		}
	}
	return info
}

// getRemainingTraffic 获取剩余流量（字节）
func (si *SubscriptionInfo) getRemainingTraffic() int64 {
	if si == nil || si.Total == 0 {
		return -1 // 未知
	}
	return si.Total - si.Upload - si.Download
}
func (pm *ProxyManager) loadFromURL(urlStr string) ([]*ProxyNode, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(urlStr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 检查订阅流量信息
	userinfo := resp.Header.Get("subscription-userinfo")
	if userinfo == "" {
		userinfo = resp.Header.Get("Subscription-Userinfo")
	}
	if subInfo := parseSubscriptionUserinfo(userinfo); subInfo != nil {
		remaining := subInfo.getRemainingTraffic()
		if remaining == 0 {
			return nil, fmt.Errorf("订阅流量已耗尽")
		}
		if remaining > 0 && remaining < 100*1024*1024 {
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return pm.parseContent(string(body))
}

// loadFromFile 从文件加载
func (pm *ProxyManager) loadFromFile(path string) ([]*ProxyNode, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return pm.parseContent(string(data))
}

func (pm *ProxyManager) parseContent(content string) ([]*ProxyNode, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(content))
	if err == nil {
		content = string(decoded)
	}

	var nodes []*ProxyNode
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		node := pm.parseLine(line)
		if node != nil {
			nodes = append(nodes, node)
		}
	}

	return nodes, nil
}

// tryBase64Decode 尝试多种 base64 解码方式
func tryBase64Decode(s string) []byte {
	s = strings.TrimSpace(s)
	// 尝试标准 base64
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil {
		return decoded
	}
	// 尝试 URL-safe base64
	if decoded, err := base64.URLEncoding.DecodeString(s); err == nil {
		return decoded
	}
	// 尝试无填充的标准 base64
	if decoded, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return decoded
	}
	// 尝试无填充的 URL-safe base64
	if decoded, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return decoded
	}
	return nil
}

// parseLine 解析单行
func (pm *ProxyManager) parseLine(line string) *ProxyNode {
	if strings.HasPrefix(line, "vmess://") {
		return parseVmess(line)
	}
	if strings.HasPrefix(line, "vless://") {
		return parseVless(line)
	}
	if strings.HasPrefix(line, "ss://") {
		return parseSS(line)
	}
	if strings.HasPrefix(line, "trojan://") {
		return parseTrojan(line)
	}
	if strings.HasPrefix(line, "hysteria2://") || strings.HasPrefix(line, "hy2://") {
		return parseHysteria2(line)
	}
	if strings.HasPrefix(line, "anytls://") {
		return parseAnyTLS(line)
	}
	if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") || strings.HasPrefix(line, "socks5://") {
		return parseDirectProxy(line)
	}
	return nil
}

// getStringFromMap 安全获取 map 中的字符串值
func getStringFromMap(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		switch s := v.(type) {
		case string:
			return s
		case float64:
			return strconv.FormatFloat(s, 'f', -1, 64)
		case int:
			return strconv.Itoa(s)
		}
	}
	return ""
}

// getIntFromMap 安全获取 map 中的整数值
func getIntFromMap(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			i, _ := strconv.Atoi(n)
			return i
		}
	}
	return 0
}

// parseVmess 解析 vmess 链接
func parseVmess(link string) *ProxyNode {
	// vmess://base64(json)
	data := strings.TrimPrefix(link, "vmess://")
	decoded := tryBase64Decode(data)
	if decoded == nil {
		return nil
	}

	var config map[string]interface{}
	if err := json.Unmarshal(decoded, &config); err != nil {
		return nil
	}

	node := &ProxyNode{
		Raw:      link,
		Protocol: "vmess",
	}

	node.Name = getStringFromMap(config, "ps")
	node.Server = getStringFromMap(config, "add")
	node.Port = getIntFromMap(config, "port")
	node.UUID = getStringFromMap(config, "id")
	node.AlterId = getIntFromMap(config, "aid")

	// 加密方式
	node.Security = getStringFromMap(config, "scy")
	if node.Security == "" {
		node.Security = "auto"
	}

	// 传输协议
	node.Network = getStringFromMap(config, "net")
	if node.Network == "" {
		node.Network = "tcp"
	}

	// 路径和 Host
	node.Path = getStringFromMap(config, "path")
	node.Host = getStringFromMap(config, "host")

	// TLS 设置（支持多种写法）
	tlsVal := getStringFromMap(config, "tls")
	if tlsVal != "" && tlsVal != "none" && tlsVal != "0" && tlsVal != "false" {
		node.TLS = true
	}
	node.SNI = getStringFromMap(config, "sni")
	if node.SNI == "" && node.TLS {
		node.SNI = node.Host
	}

	// Header 类型（kcp/quic）
	node.Type = getStringFromMap(config, "type")

	if node.Server == "" || node.Port == 0 || node.UUID == "" {
		return nil
	}
	return node
}

// parseVless 解析 vless 链接
func parseVless(link string) *ProxyNode {
	// vless://uuid@server:port?params#name
	u, err := url.Parse(link)
	if err != nil {
		return nil
	}

	port, _ := strconv.Atoi(u.Port())
	// URL 解码名称
	name, _ := url.QueryUnescape(u.Fragment)

	node := &ProxyNode{
		Raw:      link,
		Protocol: "vless",
		UUID:     u.User.Username(),
		Server:   u.Hostname(),
		Port:     port,
		Name:     name,
	}

	query := u.Query()

	// 传输协议（支持更多类型）
	node.Network = query.Get("type")
	if node.Network == "" {
		node.Network = "tcp"
	}

	// 安全类型
	node.Security = query.Get("security")
	if node.Security == "" {
		node.Security = "none"
	}
	if node.Security == "tls" || node.Security == "reality" {
		node.TLS = true
	}

	// Flow（XTLS）
	node.Flow = query.Get("flow")

	// 路径（需要 URL 解码）
	if path := query.Get("path"); path != "" {
		node.Path, _ = url.QueryUnescape(path)
	}

	// Host
	node.Host = query.Get("host")
	if node.Host == "" {
		node.Host = query.Get("sni")
	}

	// SNI
	node.SNI = query.Get("sni")
	if node.SNI == "" && node.TLS && node.Security != "reality" {
		node.SNI = node.Host
		if node.SNI == "" {
			node.SNI = node.Server
		}
	}

	// Fingerprint（TLS/Reality 指纹）
	node.Fingerprint = query.Get("fp")
	if node.Fingerprint == "" {
		node.Fingerprint = query.Get("fingerprint")
	}

	// Reality 相关参数
	if node.Security == "reality" {
		node.PublicKey = query.Get("pbk")
		node.ShortId = query.Get("sid")
		node.SpiderX = query.Get("spx")
		// Reality 必须有 SNI
		if node.SNI == "" {
			node.SNI = query.Get("serverName")
		}
	}

	// ALPN
	node.ALPN = query.Get("alpn")

	// Header 类型（kcp/quic 等）
	node.Type = query.Get("headerType")

	// GRPC 服务名
	if serviceName := query.Get("serviceName"); serviceName != "" && node.Network == "grpc" {
		node.Path = serviceName
	}

	// xhttp/splithttp/httpupgrade 的额外参数
	if node.Network == "xhttp" || node.Network == "splithttp" || node.Network == "httpupgrade" {
		if node.Path == "" {
			node.Path = "/"
		}
	}

	if node.Server == "" || node.Port == 0 || node.UUID == "" {
		return nil
	}
	return node
}

// xray-core 支持的 shadowsocks 加密方法
var supportedSSCiphers = map[string]bool{
	// AEAD 加密（推荐）
	"aes-128-gcm":             true,
	"aes-256-gcm":             true,
	"chacha20-poly1305":       true,
	"chacha20-ietf-poly1305":  true,
	"xchacha20-poly1305":      true,
	"xchacha20-ietf-poly1305": true,
	// 流式加密（xray-core 支持）
	"aes-128-ctr": true,
	"aes-192-ctr": true,
	"aes-256-ctr": true,
	// 其他支持的
	"none":                          true,
	"plain":                         true,
	"2022-blake3-aes-128-gcm":       true,
	"2022-blake3-aes-256-gcm":       true,
	"2022-blake3-chacha20-poly1305": true,
}

// 不支持的 cipher 方法映射（旧的 CFB/OFB 等）
var unsupportedSSCiphers = map[string]string{
	"aes-128-cfb":   "", // 不支持，跳过
	"aes-192-cfb":   "",
	"aes-256-cfb":   "",
	"aes-128-ofb":   "",
	"aes-192-ofb":   "",
	"aes-256-ofb":   "",
	"bf-cfb":        "",
	"cast5-cfb":     "",
	"des-cfb":       "",
	"idea-cfb":      "",
	"rc2-cfb":       "",
	"rc4":           "",
	"rc4-md5":       "",
	"rc4-md5-6":     "",
	"seed-cfb":      "",
	"salsa20":       "",
	"chacha20":      "chacha20-ietf-poly1305", // 尝试升级
	"chacha20-ietf": "chacha20-ietf-poly1305",
}

// isSupportedSSCipher 检查是否支持的 cipher
func isSupportedSSCipher(method string) bool {
	method = strings.ToLower(method)
	return supportedSSCiphers[method]
}

// tryMapSSCipher 尝试映射不支持的 cipher 到支持的
func tryMapSSCipher(method string) (string, bool) {
	method = strings.ToLower(method)
	if isSupportedSSCipher(method) {
		return method, true
	}
	if mapped, ok := unsupportedSSCiphers[method]; ok {
		if mapped == "" {
			return "", false // 不支持且无法映射
		}
		return mapped, true
	}
	return "", false
}

// parseSS 解析 ss 链接
func parseSS(link string) *ProxyNode {
	// 支持多种格式:
	// ss://base64(method:password)@host:port#name (SIP002)
	// ss://base64(method:password@host:port)#name (旧格式)
	// ss://method:password@host:port#name (明文格式)
	origLink := link
	link = strings.TrimPrefix(link, "ss://")

	var name string
	if idx := strings.Index(link, "#"); idx != -1 {
		name = link[idx+1:]
		link = link[:idx]
	}
	name, _ = url.QueryUnescape(name)

	node := &ProxyNode{
		Protocol: "shadowsocks",
		Name:     name,
	}

	// 尝试解析 SIP002 格式: base64(method:password)@host:port
	if atIdx := strings.LastIndex(link, "@"); atIdx != -1 {
		userInfo := link[:atIdx]
		hostPort := link[atIdx+1:]

		// 尝试 base64 解码 userInfo
		if decoded := tryBase64Decode(userInfo); decoded != nil {
			parts := strings.SplitN(string(decoded), ":", 2)
			if len(parts) == 2 {
				node.Method = parts[0]
				node.Password = parts[1]
			}
		} else {
			// 可能是明文格式 method:password
			parts := strings.SplitN(userInfo, ":", 2)
			if len(parts) == 2 {
				node.Method = parts[0]
				node.Password = parts[1]
			}
		}

		// 解析 host:port（可能包含 IPv6）
		if strings.HasPrefix(hostPort, "[") {
			// IPv6: [::1]:port
			if endBracket := strings.Index(hostPort, "]:"); endBracket != -1 {
				node.Server = hostPort[1:endBracket]
				node.Port, _ = strconv.Atoi(hostPort[endBracket+2:])
			}
		} else {
			parts := strings.Split(hostPort, ":")
			if len(parts) >= 2 {
				node.Server = parts[0]
				node.Port, _ = strconv.Atoi(parts[len(parts)-1])
			}
		}
	} else {
		// 旧格式: 整个内容是 base64 编码
		decoded := tryBase64Decode(link)
		if decoded == nil {
			return nil
		}
		// method:password@host:port
		decodedStr := string(decoded)
		if atIdx := strings.LastIndex(decodedStr, "@"); atIdx != -1 {
			userInfo := decodedStr[:atIdx]
			hostPort := decodedStr[atIdx+1:]

			parts := strings.SplitN(userInfo, ":", 2)
			if len(parts) == 2 {
				node.Method = parts[0]
				node.Password = parts[1]
			}

			hpParts := strings.Split(hostPort, ":")
			if len(hpParts) >= 2 {
				node.Server = hpParts[0]
				node.Port, _ = strconv.Atoi(hpParts[len(hpParts)-1])
			}
		}
	}

	node.Raw = origLink
	// 验证必要字段
	if node.Server == "" || node.Port == 0 || node.Method == "" {
		return nil
	}

	// 检查并映射 cipher 方法
	if mappedMethod, ok := tryMapSSCipher(node.Method); ok {
		if mappedMethod != node.Method {
			node.Method = mappedMethod
		}
	} else {
		return nil // 跳过不支持的节点
	}
	return node
}

// parseTrojan 解析 trojan 链接
func parseTrojan(link string) *ProxyNode {
	// trojan://password@server:port?params#name
	u, err := url.Parse(link)
	if err != nil {
		return nil
	}

	port, _ := strconv.Atoi(u.Port())
	name, _ := url.QueryUnescape(u.Fragment)
	node := &ProxyNode{
		Raw:      link,
		Protocol: "trojan",
		Password: u.User.Username(),
		Server:   u.Hostname(),
		Port:     port,
		Name:     name,
		TLS:      true, // trojan 默认 TLS
	}

	query := u.Query()
	node.SNI = query.Get("sni")
	if node.SNI == "" {
		node.SNI = node.Server
	}
	if host := query.Get("host"); host != "" {
		node.Host = host
	}

	// 传输协议
	node.Network = query.Get("type")
	if node.Network == "" {
		node.Network = "tcp"
	}

	// 路径
	if path := query.Get("path"); path != "" {
		node.Path, _ = url.QueryUnescape(path)
	}

	// Fingerprint
	node.Fingerprint = query.Get("fp")

	// ALPN
	node.ALPN = query.Get("alpn")

	if node.Server == "" || node.Port == 0 || node.Password == "" {
		return nil
	}
	return node
}

// parseHysteria2 解析 hysteria2/hy2 链接
func parseHysteria2(link string) *ProxyNode {
	// hysteria2://password@server:port?params#name
	// hy2://password@server:port?params#name
	link = strings.Replace(link, "hy2://", "hysteria2://", 1)
	u, err := url.Parse(link)
	if err != nil {
		return nil
	}

	port, _ := strconv.Atoi(u.Port())
	name, _ := url.QueryUnescape(u.Fragment)

	node := &ProxyNode{
		Raw:      link,
		Protocol: "hysteria2",
		Password: u.User.Username(),
		Server:   u.Hostname(),
		Port:     port,
		Name:     name,
		TLS:      true, // hysteria2 默认 TLS
	}

	query := u.Query()
	node.SNI = query.Get("sni")
	if node.SNI == "" {
		node.SNI = node.Server
	}

	// ALPN
	node.ALPN = query.Get("alpn")
	if node.ALPN == "" {
		node.ALPN = "h3"
	}

	// Fingerprint
	node.Fingerprint = query.Get("pinSHA256")

	// obfs
	if obfs := query.Get("obfs"); obfs != "" {
		node.Type = obfs
		node.Path = query.Get("obfs-password")
	}

	if node.Server == "" || node.Port == 0 || node.Password == "" {
		return nil
	}
	return node
}

// parseAnyTLS 解析 anytls 链接
func parseAnyTLS(link string) *ProxyNode {
	// anytls://password@server:port?params#name
	u, err := url.Parse(link)
	if err != nil {
		return nil
	}

	port, _ := strconv.Atoi(u.Port())
	name, _ := url.QueryUnescape(u.Fragment)

	node := &ProxyNode{
		Raw:      link,
		Protocol: "anytls",
		Password: u.User.Username(),
		Server:   u.Hostname(),
		Port:     port,
		Name:     name,
		TLS:      true,
	}

	query := u.Query()
	node.SNI = query.Get("sni")
	if node.SNI == "" {
		node.SNI = query.Get("serverName")
	}
	if node.SNI == "" {
		node.SNI = node.Server
	}

	// Fingerprint
	node.Fingerprint = query.Get("fp")
	if node.Fingerprint == "" {
		node.Fingerprint = query.Get("fingerprint")
	}

	// ALPN
	node.ALPN = query.Get("alpn")

	// insecure
	if query.Get("allowInsecure") == "1" || query.Get("insecure") == "1" {
		// 标记跳过证书验证
	}

	if node.Server == "" || node.Port == 0 || node.Password == "" {
		return nil
	}
	return node
}

// parseDirectProxy 解析直接代理
func parseDirectProxy(link string) *ProxyNode {
	u, err := url.Parse(link)
	if err != nil {
		return nil
	}

	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		if u.Scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	}

	return &ProxyNode{
		Raw:       link,
		Protocol:  u.Scheme,
		Server:    u.Hostname(),
		Port:      port,
		LocalPort: port, // 直接代理使用原端口
		Healthy:   true,
	}
}

// startInstanceLocked 内部方法：启动实例（使用 sing-box）
func (pm *ProxyManager) startInstanceLocked(node *ProxyNode) (*ProxyInstance, error) {
	// 直接代理不需要启动
	if node.Protocol == "http" || node.Protocol == "https" || node.Protocol == "socks5" {
		return &ProxyInstance{
			node:     node,
			running:  true,
			status:   InstanceStatusIdle,
			proxyURL: node.Raw,
			lastUsed: time.Now(),
		}, nil
	}

	// 使用 sing-box 启动代理
	proxyURL, err := singboxMgr.Start(node)
	if err != nil {
		return nil, fmt.Errorf("sing-box 启动失败: %w", err)
	}

	return &ProxyInstance{
		node:      node,
		running:   true,
		status:    InstanceStatusIdle,
		proxyURL:  proxyURL,
		lastUsed:  time.Now(),
		localPort: node.LocalPort,
	}, nil
}

func (pm *ProxyManager) StartXray(node *ProxyNode) (string, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	instance, err := pm.startInstanceLocked(node)
	if err != nil {
		return "", err
	}
	return instance.proxyURL, nil
}

// StopProxy 停止代理实例
func (pm *ProxyManager) StopProxy(localPort int) {
	singboxMgr.Stop(localPort)
}

// StopXray 停止代理实例（兼容旧接口）
func (pm *ProxyManager) StopXray(localPort int) {
	pm.StopProxy(localPort)
}

// StopAll 停止所有实例
func (pm *ProxyManager) StopAll() {
	singboxMgr.StopAll()
	log.Printf("🛑 所有代理实例已停止")
}

// CheckHealth 检查节点健康并获取出口IP
func (pm *ProxyManager) CheckHealth(node *ProxyNode) bool {
	proxyURL, err := pm.StartXray(node)
	if err != nil {
		return false
	}
	defer func() {
		if node.Protocol != "http" && node.Protocol != "https" && node.Protocol != "socks5" {
			pm.StopXray(node.LocalPort)
		}
	}()

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	if proxyURL != "" {
		proxy, _ := url.Parse(proxyURL)
		transport.Proxy = http.ProxyURL(proxy)
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   HealthCheckTimeout,
	}

	// 第一步：基本连通性检查
	start := time.Now()
	resp, err := client.Get(pm.healthCheckURL)
	if err != nil {
		return false
	}
	resp.Body.Close()

	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		return false
	}
	node.Latency = time.Since(start)
	ipClient := &http.Client{
		Transport: transport,
		Timeout:   3 * time.Second,
	}
	ipResp, err := ipClient.Get("https://ipinfo.io/ip")
	if err != nil {
		return true
	}
	defer ipResp.Body.Close()

	if ipResp.StatusCode == 200 {
		ipBytes, _ := io.ReadAll(ipResp.Body)
		node.ExitIP = strings.TrimSpace(string(ipBytes))
	}

	return true
}
func (pm *ProxyManager) CheckHealthQuick(node *ProxyNode) bool {
	proxyURL, err := pm.StartXray(node)
	if err != nil {
		return false
	}
	defer func() {
		if node.Protocol != "http" && node.Protocol != "https" && node.Protocol != "socks5" {
			pm.StopXray(node.LocalPort)
		}
	}()

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	if proxyURL != "" {
		proxy, _ := url.Parse(proxyURL)
		transport.Proxy = http.ProxyURL(proxy)
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	start := time.Now()
	resp, err := client.Get(pm.healthCheckURL)
	if err != nil {
		return false
	}
	resp.Body.Close()
	node.Latency = time.Since(start)

	return resp.StatusCode == 204 || resp.StatusCode == 200
}

func (pm *ProxyManager) CheckAllHealth() {
	pm.mu.Lock()
	if pm.healthChecking {
		pm.mu.Unlock()
		return
	}
	pm.healthChecking = true
	hasSubscribes := len(pm.subscribeURLs) > 0
	pm.mu.Unlock()
	if hasSubscribes {
		if err := pm.LoadAll(); err != nil {
			log.Printf("⚠️ 刷新订阅失败: %v", err)
		}
	}

	pm.mu.Lock()
	nodes := make([]*ProxyNode, len(pm.nodes))
	copy(nodes, pm.nodes)
	pm.mu.Unlock()

	if len(nodes) == 0 {
		pm.mu.Lock()
		pm.healthChecking = false
		pm.mu.Unlock()
		pm.SetReady(true)
		return
	}
	var healthyNodes, unhealthyNodes, newNodes []*ProxyNode
	for _, n := range nodes {
		if n.LastCheck.IsZero() {
			newNodes = append(newNodes, n)
		} else if n.Healthy {
			healthyNodes = append(healthyNodes, n)
		} else {
			unhealthyNodes = append(unhealthyNodes, n)
		}
	}
	var healthy []*ProxyNode
	var checked int32
	var mainWg sync.WaitGroup
	var mu sync.Mutex
	ipSeen := make(map[string]bool)
	total := len(nodes)

	// 检查单个节点的函数
	checkNode := func(n *ProxyNode, sem chan struct{}) {
		sem <- struct{}{}
		defer func() { <-sem }()

		n.Healthy = pm.CheckHealth(n)
		n.LastCheck = time.Now()

		current := int(atomic.AddInt32(&checked, 1))

		mu.Lock()
		if n.Healthy {
			if n.ExitIP != "" {
				if ipSeen[n.ExitIP] {
					for i, existing := range healthy {
						if existing.ExitIP == n.ExitIP && n.Latency < existing.Latency {
							healthy[i] = n
							break
						}
					}
				} else {
					ipSeen[n.ExitIP] = true
					healthy = append(healthy, n)
				}
			} else {
				healthy = append(healthy, n)
			}
			if len(healthy) >= MinHealthyForReady {
				pm.mu.Lock()
				if !pm.ready {
					pm.ready = true
					pm.healthyNodes = healthy
					pm.readyCond.Broadcast()
				}
				pm.mu.Unlock()
			}
		}
		healthyCount := len(healthy)
		mu.Unlock()

		if current%50 == 0 || current == total {
			log.Printf("🔍 进度: %d/%d, 健康: %d", current, total, healthyCount)
		}
	}
	mainWg.Add(1)
	go func() {
		defer mainWg.Done()
		var wg sync.WaitGroup
		sem := make(chan struct{}, 32)
		for _, n := range healthyNodes {
			wg.Add(1)
			go func(node *ProxyNode) {
				defer wg.Done()
				checkNode(node, sem)
			}(n)
		}
		wg.Wait()
	}()
	mainWg.Add(1)
	go func() {
		defer mainWg.Done()
		var wg sync.WaitGroup
		sem := make(chan struct{}, 32)
		for _, n := range unhealthyNodes {
			wg.Add(1)
			go func(node *ProxyNode) {
				defer wg.Done()
				checkNode(node, sem)
			}(n)
		}
		wg.Wait()
	}()
	mainWg.Add(1)
	go func() {
		defer mainWg.Done()
		var wg sync.WaitGroup
		sem := make(chan struct{}, 32)
		for _, n := range newNodes {
			wg.Add(1)
			go func(node *ProxyNode) {
				defer wg.Done()
				checkNode(node, sem)
			}(n)
		}
		wg.Wait()
	}()

	mainWg.Wait()

	// 按延迟排序（延迟低的排前面）
	sort.Slice(healthy, func(i, j int) bool {
		return healthy[i].Latency < healthy[j].Latency
	})

	pm.mu.Lock()
	pm.healthyNodes = healthy
	pm.healthChecking = false
	// 只有达到最少健康节点数才提示就绪
	pm.ready = len(healthy) >= MinHealthyForReady
	pm.readyCond.Broadcast()
	pm.mu.Unlock()

	// 输出健康检查结果
	uniqueIPs := len(ipSeen)
	if len(healthy) > 0 {
		topN := 5
		if len(healthy) < topN {
			topN = len(healthy)
		}
		log.Printf("✅ 健康检查完成: %d/%d 节点可用 ",
			len(healthy), total)
		log.Printf("📊 最快前%d节点: %v ~ %v",
			topN, healthy[0].Latency.Round(time.Millisecond),
			healthy[topN-1].Latency.Round(time.Millisecond))

		// 输出IP分布信息
		if uniqueIPs < len(healthy) {
		}
	} else {
		log.Printf("⚠️ 健康检查完成: 0/%d 节点可用", total)
	}

	// 就绪状态提示
	if pm.ready {
		log.Printf("🟢 代理池就绪 (健康节点: %d >= 最低要求: %d)", len(healthy), MinHealthyForReady)
	} else {
		log.Printf("🔴 代理池未就绪 (健康节点: %d < 最低要求: %d)", len(healthy), MinHealthyForReady)
	}
}

// GetFromPool 从实例池获取一个空闲实例
func (pm *ProxyManager) GetFromPool() *ProxyInstance {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// 查找空闲实例
	for _, inst := range pm.instancePool {
		inst.mu.Lock()
		if inst.status == InstanceStatusIdle && inst.running {
			inst.status = InstanceStatusInUse
			inst.lastUsed = time.Now()
			inst.mu.Unlock()
			return inst
		}
		inst.mu.Unlock()
	}
	return nil
}

// ReturnToPool 归还实例到池
func (pm *ProxyManager) ReturnToPool(inst *ProxyInstance) {
	if inst == nil {
		return
	}
	inst.mu.Lock()
	inst.status = InstanceStatusIdle
	inst.mu.Unlock()
}

// ReleaseByURL 通过proxyURL停止并释放实例
func (pm *ProxyManager) ReleaseByURL(proxyURL string) {
	pm.mu.Lock()
	// 查找并移除实例
	var toStop *ProxyInstance
	for i, inst := range pm.instancePool {
		inst.mu.Lock()
		if inst.proxyURL == proxyURL {
			toStop = inst
			// 从池中移除
			pm.instancePool = append(pm.instancePool[:i], pm.instancePool[i+1:]...)
			inst.mu.Unlock()
			break
		}
		inst.mu.Unlock()
	}
	pm.mu.Unlock()

	// 停止实例（在锁外执行）
	if toStop != nil {
		pm.StopXray(toStop.localPort)
	}
}

func (pm *ProxyManager) Next() string {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if len(pm.healthyNodes) == 0 && len(pm.nodes) == 0 {
		return ""
	}

	now := time.Now()
	var selectedNode *ProxyNode
	var selectedIdx int = -1

	// 从健康节点列表中找第一个可用的
	for i, node := range pm.healthyNodes {
		// 检查冷却时间
		cooldown := node.UseCooldown
		if cooldown == 0 {
			cooldown = DefaultProxyUseCooldown
		}
		if now.Sub(node.LastUsed) < cooldown {
			continue
		}
		// 跳过失败次数过多的节点
		if node.FailCount >= MaxProxyFailCount {
			continue
		}
		selectedNode = node
		selectedIdx = i
		break
	}

	// 如果健康节点都不可用，尝试普通节点
	if selectedNode == nil {
		for i, node := range pm.nodes {
			cooldown := node.UseCooldown
			if cooldown == 0 {
				cooldown = DefaultProxyUseCooldown
			}
			if now.Sub(node.LastUsed) < cooldown {
				continue
			}
			if node.FailCount >= MaxProxyFailCount {
				continue
			}
			selectedNode = node
			selectedIdx = i
			break
		}
	}

	// 如果所有节点都在冷却中，选择最久未用的健康节点
	if selectedNode == nil {
		var oldest *ProxyNode
		var oldestIdx int
		for i, node := range pm.healthyNodes {
			if oldest == nil || node.LastUsed.Before(oldest.LastUsed) {
				oldest = node
				oldestIdx = i
			}
		}
		if oldest == nil {
			for i, node := range pm.nodes {
				if oldest == nil || node.LastUsed.Before(oldest.LastUsed) {
					oldest = node
					oldestIdx = i
				}
			}
		}
		if oldest == nil {
			return ""
		}
		selectedNode = oldest
		selectedIdx = oldestIdx
	}

	selectedNode.LastUsed = now
	isFromHealthy := false
	for i, node := range pm.healthyNodes {
		if node == selectedNode {
			isFromHealthy = true
			selectedIdx = i
			break
		}
	}

	if isFromHealthy && selectedIdx >= 0 && selectedIdx < len(pm.healthyNodes) {
		// 从健康节点列表移动到末尾
		pm.healthyNodes = append(pm.healthyNodes[:selectedIdx], pm.healthyNodes[selectedIdx+1:]...)
		pm.healthyNodes = append(pm.healthyNodes, selectedNode)
	} else if !isFromHealthy {
		// 从普通节点列表移动到末尾
		for i, node := range pm.nodes {
			if node == selectedNode {
				pm.nodes = append(pm.nodes[:i], pm.nodes[i+1:]...)
				pm.nodes = append(pm.nodes, selectedNode)
				break
			}
		}
	}

	// 启动新实例
	instance, err := pm.startInstanceLocked(selectedNode)
	if err != nil {
		log.Printf("⚠️ 启动代理失败: %v", err)
		selectedNode.FailCount++
		// 失败后增加冷却时间
		selectedNode.UseCooldown = time.Duration(selectedNode.FailCount) * 10 * time.Second
		return ""
	}
	instance.status = InstanceStatusInUse

	// 始终追踪实例（用于 MarkProxyFailed/ReleaseByURL）
	pm.instancePool = append(pm.instancePool, instance)
	return instance.proxyURL
}

// MarkProxyFailed 标记代理失败（如403等）
func (pm *ProxyManager) MarkProxyFailed(proxyURL string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// 从实例池找到对应节点
	for _, inst := range pm.instancePool {
		if inst.proxyURL == proxyURL && inst.node != nil {
			inst.node.FailCount++
			// 失败后动态增加冷却时间
			inst.node.UseCooldown = time.Duration(inst.node.FailCount) * 15 * time.Second
			if inst.node.UseCooldown > 2*time.Minute {
				inst.node.UseCooldown = 2 * time.Minute
			}
			log.Printf("⚠️ 代理失败标记: %s, 失败次数=%d, 冷却=%v",
				inst.node.Name, inst.node.FailCount, inst.node.UseCooldown)
			return
		}
	}
}

// MarkProxySuccess 标记代理成功（重置失败计数）
func (pm *ProxyManager) MarkProxySuccess(proxyURL string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, inst := range pm.instancePool {
		if inst.proxyURL == proxyURL && inst.node != nil {
			inst.node.FailCount = 0
			inst.node.UseCooldown = DefaultProxyUseCooldown
			return
		}
	}
}

// PoolStats 返回实例池统计
func (pm *ProxyManager) PoolStats() map[string]int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	idle, inUse := 0, 0
	for _, inst := range pm.instancePool {
		inst.mu.Lock()
		switch inst.status {
		case InstanceStatusIdle:
			idle++
		case InstanceStatusInUse:
			inUse++
		}
		inst.mu.Unlock()
	}
	return map[string]int{
		"idle":   idle,
		"in_use": inUse,
		"total":  len(pm.instancePool),
	}
}

// Count 获取代理数量
func (pm *ProxyManager) Count() int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if len(pm.healthyNodes) > 0 {
		return len(pm.healthyNodes)
	}
	return len(pm.nodes)
}

// HealthyCount 获取健康代理数量
func (pm *ProxyManager) HealthyCount() int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.healthyNodes)
}

// TotalCount 获取总代理数量
func (pm *ProxyManager) TotalCount() int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.nodes)
}

// StartAutoUpdate 启动自动更新和健康检查
func (pm *ProxyManager) StartAutoUpdate() {
	// 自动更新订阅
	go func() {
		for {
			time.Sleep(pm.updateInterval)
			if len(pm.subscribeURLs) > 0 || len(pm.proxyFiles) > 0 {
				if err := pm.LoadAll(); err != nil {
					log.Printf("⚠️ 自动更新代理失败: %v", err)
				}
			}
		}
	}()

	// 后台健康检查（启动时立即开始，不阻塞）
	go func() {
		// 延迟几秒后开始首次检查
		time.Sleep(3 * time.Second)
		pm.CheckAllHealth()

		// 定期检查
		for {
			time.Sleep(pm.checkInterval)
			pm.CheckAllHealth()
		}
	}()
}

// SetProxies 直接设置代理（兼容旧接口）
func (pm *ProxyManager) SetProxies(proxies []string) {
	var nodes []*ProxyNode
	for _, p := range proxies {
		if node := pm.parseLine(p); node != nil {
			nodes = append(nodes, node)
		}
	}
	pm.mu.Lock()
	pm.nodes = nodes
	pm.healthyNodes = nodes // 假设都健康
	pm.mu.Unlock()
	log.Printf("✅ 代理池已设置 %d 个代理", len(nodes))
}

const (
	autoRegisterURL      = "https://jgpyjc.top/api/v1/passport/auth/register"
	autoSubscribeBaseURL = "https://bb1.jgpyjc.top/api/v1/client/subscribe?token="
	autoRegisterInterval = 1 * time.Hour
)

// AutoSubscriber 自动订阅管理器
type AutoSubscriber struct {
	mu              sync.RWMutex
	currentToken    string
	subscribeURL    string
	lastRefresh     time.Time
	running         bool
	stopChan        chan struct{}
	proxyManager    *ProxyManager
	refreshInterval time.Duration
}

var autoSubscriber = &AutoSubscriber{
	refreshInterval: autoRegisterInterval,
	stopChan:        make(chan struct{}),
}

// randString 生成随机字符串
func randString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		r, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		out[i] = letters[r.Int64()]
	}
	return string(out)
}

// ungzipIfNeeded 解压 gzip 数据
func ungzipIfNeeded(data []byte, header http.Header) ([]byte, error) {
	ce := strings.ToLower(header.Get("Content-Encoding"))
	if ce == "gzip" || (len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b) {
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	}
	return data, nil
}

// extractToken 从响应中提取 token
func extractToken(body []byte) string {
	var j interface{}
	if err := json.Unmarshal(body, &j); err != nil {
		return ""
	}

	var walk func(interface{}) string
	walk = func(x interface{}) string {
		switch v := x.(type) {
		case map[string]interface{}:
			for _, key := range []string{"token", "access_token", "data", "result", "auth", "jwt"} {
				if val, ok := v[key]; ok {
					if s, ok2 := val.(string); ok2 && s != "" {
						return s
					}
					if res := walk(val); res != "" {
						return res
					}
				}
			}
			// 检查 JWT 格式
			for _, val := range v {
				if s, ok := val.(string); ok && looksLikeJWT(s) {
					return s
				}
			}
		case []interface{}:
			for _, item := range v {
				if res := walk(item); res != "" {
					return res
				}
			}
		}
		return ""
	}
	return walk(j)
}

// looksLikeJWT 判断是否像 JWT
func looksLikeJWT(s string) bool {
	parts := strings.Count(s, ".")
	return parts >= 2 && len(s) > 30
}

// 常见邮箱域名
var emailDomains = []string{
	"gmail.com", "yahoo.com", "outlook.com", "hotmail.com", "icloud.com",
	"protonmail.com", "mail.com", "zoho.com", "aol.com", "yandex.com",
	"163.com", "qq.com", "126.com", "sina.com", "foxmail.com",
}

// doAutoRegister 执行一次自动注册
func doAutoRegister() (email, password, token string, err error) {
	// 随机邮箱：随机用户名 + 随机域名
	domainIdx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(emailDomains))))
	email = randString(8+int(domainIdx.Int64()%5)) + "@" + emailDomains[domainIdx.Int64()]
	password = randString(20)

	form := url.Values{}
	form.Set("email", email)
	form.Set("password", password)
	form.Set("invite_code", "odtRDsfd")
	form.Set("email_code", "")

	req, err := http.NewRequest("POST", autoRegisterURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", "", err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 10)")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://jgpyjc.top")
	req.Header.Set("Referer", "https://jgpyjc.top/")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return email, password, "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return email, password, "", err
	}

	body, err := ungzipIfNeeded(raw, resp.Header)
	if err != nil {
		body = raw
	}

	token = extractToken(body)
	if token == "" {
		s := strings.TrimSpace(string(body))
		if looksLikeJWT(s) {
			token = s
		}
	}

	if token == "" {
		return email, password, "", fmt.Errorf("未能从响应中提取 token: %s", string(body[:min(200, len(body))]))
	}
	return email, password, token, nil
}

// refreshSubscription 刷新订阅
func (as *AutoSubscriber) refreshSubscription() error {

	_, _, token, err := doAutoRegister()
	if err != nil {
		return fmt.Errorf("注册失败: %w", err)
	}

	subscribeURL := autoSubscribeBaseURL + token

	as.mu.Lock()
	as.currentToken = token
	as.subscribeURL = subscribeURL
	as.lastRefresh = time.Now()
	as.mu.Unlock()
	// 加载订阅到代理池
	if as.proxyManager != nil {
		if err := as.loadToProxyManager(); err != nil {
		}
	}

	return nil
}

func (as *AutoSubscriber) loadToProxyManager() error {
	as.mu.RLock()
	subURL := as.subscribeURL
	as.mu.RUnlock()

	if subURL == "" {
		return fmt.Errorf("订阅URL为空")
	}

	nodes, err := as.proxyManager.loadFromURL(subURL)
	if err != nil {
		return err
	}

	if len(nodes) == 0 {
		return fmt.Errorf("订阅中没有可用节点")
	}

	as.proxyManager.mu.Lock()
	as.proxyManager.nodes = append(as.proxyManager.nodes, nodes...)
	as.proxyManager.mu.Unlock()
	go as.proxyManager.CheckAllHealth()

	return nil
}
func (as *AutoSubscriber) Start(pm *ProxyManager) {
	as.mu.Lock()
	if as.running {
		as.mu.Unlock()
		return
	}
	as.running = true
	as.proxyManager = pm
	as.stopChan = make(chan struct{})
	as.mu.Unlock()
	go func() {
		if err := as.refreshSubscription(); err != nil {
		}

		ticker := time.NewTicker(as.refreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-as.stopChan:
				return
			case <-ticker.C:
				if err := as.refreshSubscription(); err != nil {
				}
			}
		}
	}()
}

func (as *AutoSubscriber) Stop() {
	as.mu.Lock()
	defer as.mu.Unlock()

	if as.running {
		close(as.stopChan)
		as.running = false
	}
}

func (as *AutoSubscriber) GetCurrentSubscribeURL() string {
	as.mu.RLock()
	defer as.mu.RUnlock()
	return as.subscribeURL
}

func (as *AutoSubscriber) GetCurrentToken() string {
	as.mu.RLock()
	defer as.mu.RUnlock()
	return as.currentToken
}
func (as *AutoSubscriber) IsExpired() bool {
	as.mu.RLock()
	defer as.mu.RUnlock()
	return time.Since(as.lastRefresh) > 2*time.Hour
}
func (pm *ProxyManager) StartAutoSubscribe() {
	autoSubscriber.Start(pm)
}
func (pm *ProxyManager) StopAutoSubscribe() {
	autoSubscriber.Stop()
}
func (pm *ProxyManager) GetAutoSubscribeURL() string {
	return autoSubscriber.GetCurrentSubscribeURL()
}
func (pm *ProxyManager) HasAutoSubscribe() bool {
	return autoSubscriber.GetCurrentToken() != ""
}
