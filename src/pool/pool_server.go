package pool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"business2api/src/logger"

	"github.com/gorilla/websocket"
)

// ==================== 号池服务器（C/S架构 + WebSocket） ====================
// Server: 管理端 - 负责API服务、账号分配、状态监控
// Client: 工作端 - 负责注册新账号、401账号Cookie续期

// PoolServerConfig 号池服务器配置
type PoolServerConfig struct {
	Enable        bool   `json:"enable"`         // 是否启用分离模式
	Mode          string `json:"mode"`           // 模式: "server" 或 "client"
	ServerAddr    string `json:"server_addr"`    // 服务器地址（客户端模式使用）
	ListenAddr    string `json:"listen_addr"`    // WebSocket监听地址（服务端模式使用）
	Secret        string `json:"secret"`         // 通信密钥
	TargetCount   int    `json:"target_count"`   // 目标账号数量
	DataDir       string `json:"data_dir"`       // 数据目录
	ClientThreads int    `json:"client_threads"` // 客户端并发线程数
	ExpiredAction string `json:"expired_action"` // 账号过期处理: "delete"=删除, "refresh"=浏览器刷新, "queue"=排队等待
}

// WSMessageType WebSocket消息类型
type WSMessageType string

const (
	// Server -> Client
	WSMsgTaskRegister WSMessageType = "task_register" // 分配注册任务
	WSMsgTaskRefresh  WSMessageType = "task_refresh"  // 分配Cookie续期任务
	WSMsgHeartbeat    WSMessageType = "heartbeat"     // 心跳
	WSMsgStatus       WSMessageType = "status"        // 状态同步

	// Client -> Server
	WSMsgRegisterResult WSMessageType = "register_result" // 注册结果
	WSMsgRefreshResult  WSMessageType = "refresh_result"  // 续期结果
	WSMsgHeartbeatAck   WSMessageType = "heartbeat_ack"   // 心跳响应
	WSMsgClientReady    WSMessageType = "client_ready"    // 客户端就绪
	WSMsgRequestTask    WSMessageType = "request_task"    // 请求任务
)

// 版本信息
const (
	ProtocolVersion = "1.0"
	ServerVersion   = "2.0.0"
)

// WSMessage WebSocket消息
type WSMessage struct {
	Type      WSMessageType          `json:"type"`
	Version   string                 `json:"version,omitempty"`
	Timestamp int64                  `json:"timestamp"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// WSClient WebSocket客户端连接
type WSClient struct {
	ID            string
	Conn          *websocket.Conn
	Server        *PoolServer
	Send          chan []byte
	IsAlive       bool
	LastPing      time.Time
	MaxThreads    int    // 客户端最大线程数
	ClientVersion string // 客户端版本
	mu            sync.Mutex
}

// PoolServer 号池服务器（管理端）
type PoolServer struct {
	pool      *AccountPool
	config    PoolServerConfig
	clients   map[string]*WSClient
	clientsMu sync.RWMutex
	upgrader  websocket.Upgrader

	// 任务队列
	registerQueue chan int      // 注册任务队列
	refreshQueue  chan *Account // 续期任务队列

	// 轮询分配
	nextClientIdx int // 下一个分配任务的客户端索引
}

// NewPoolServer 创建号池服务器
func NewPoolServer(pool *AccountPool, config PoolServerConfig) *PoolServer {
	return &PoolServer{
		pool:    pool,
		config:  config,
		clients: make(map[string]*WSClient),
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(r *http.Request) bool { return true },
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		},
		registerQueue: make(chan int, 100),
		refreshQueue:  make(chan *Account, 100),
	}
}

// Start 启动号池服务器（独立端口模式，已弃用）
func (ps *PoolServer) Start() error {
	mux := http.NewServeMux()

	// 鉴权中间件
	authMiddleware := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if ps.config.Secret != "" {
				auth := r.Header.Get("X-Pool-Secret")
				if auth != ps.config.Secret {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next(w, r)
		}
	}

	// WebSocket端点
	mux.HandleFunc("/ws", ps.handleWebSocket)

	// REST API端点
	mux.HandleFunc("/pool/next", authMiddleware(ps.handleNext))
	mux.HandleFunc("/pool/mark", authMiddleware(ps.handleMark))
	mux.HandleFunc("/pool/refresh", authMiddleware(ps.handleRefresh))
	mux.HandleFunc("/pool/status", authMiddleware(ps.handleStatus))
	mux.HandleFunc("/pool/jwt", authMiddleware(ps.handleGetJWT))

	// 任务分发
	mux.HandleFunc("/pool/queue-register", authMiddleware(ps.handleQueueRegister))
	mux.HandleFunc("/pool/queue-refresh", authMiddleware(ps.handleQueueRefresh))

	// 接收账号数据（客户端回传）
	mux.HandleFunc("/pool/upload-account", authMiddleware(ps.handleUploadAccount))

	// 启动任务分发协程
	go ps.taskDispatcher()
	// 启动心跳检测
	go ps.heartbeatChecker()
	return http.ListenAndServe(ps.config.ListenAddr, mux)
}

// StartBackground 启动后台任务（任务分发和心跳检测）
func (ps *PoolServer) StartBackground() {
	go ps.taskDispatcher()
	go ps.heartbeatChecker()
}

// HandleWS 处理 WebSocket 连接（供 gin 路由使用）
func (ps *PoolServer) HandleWS(w http.ResponseWriter, r *http.Request) {
	ps.handleWebSocket(w, r)
}

func (ps *PoolServer) HandleUploadAccount(w http.ResponseWriter, r *http.Request) {
	ps.handleUploadAccount(w, r)
}
func (ps *PoolServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if ps.config.Secret != "" {
		secret := r.URL.Query().Get("secret")
		if secret != ps.config.Secret {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	conn, err := ps.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	clientID := fmt.Sprintf("client_%d", time.Now().UnixNano())
	client := &WSClient{
		ID:       clientID,
		Conn:     conn,
		Server:   ps,
		Send:     make(chan []byte, 256),
		IsAlive:  true,
		LastPing: time.Now(),
	}

	ps.clientsMu.Lock()
	ps.clients[clientID] = client
	ps.clientsMu.Unlock()

	logger.Info("[WS] 客户端连接: %s (当前: %d)", clientID, len(ps.clients))

	// 启动读写协程
	go client.writePump()
	go client.readPump()
}

// writePump 发送消息到客户端
func (c *WSClient) writePump() {
	// 缩短心跳间隔到20秒，确保连接保持活跃
	ticker := time.NewTicker(20 * time.Second)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			// 设置写入超时
			c.Conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-ticker.C:
			// 发送心跳
			msg := WSMessage{
				Type:      WSMsgHeartbeat,
				Timestamp: time.Now().Unix(),
			}
			data, _ := json.Marshal(msg)
			c.Conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := c.Conn.WriteMessage(websocket.TextMessage, data); err != nil {
				logger.Debug("[WS] 发送心跳失败: %s - %v", c.ID, err)
				return
			}
		}
	}
}

// readPump 从客户端读取消息
func (c *WSClient) readPump() {
	defer func() {
		c.Server.removeClient(c.ID)
		c.Conn.Close()
	}()

	// 延长读取超时到180秒（3分钟），以适应长时间注册任务
	c.Conn.SetReadDeadline(time.Now().Add(180 * time.Second))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(180 * time.Second))
		return nil
	})
	// 处理客户端的 Ping 消息，自动回复 Pong 并重置超时
	c.Conn.SetPingHandler(func(appData string) error {
		c.Conn.SetReadDeadline(time.Now().Add(180 * time.Second))
		c.mu.Lock()
		c.LastPing = time.Now()
		c.mu.Unlock()
		return c.Conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				logger.Debug("[WS] 读取错误: %v", err)
			}
			break
		}

		var msg WSMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		c.handleMessage(msg)
	}
}

// handleMessage 处理客户端消息
func (c *WSClient) handleMessage(msg WSMessage) {
	c.mu.Lock()
	c.LastPing = time.Now()
	c.mu.Unlock()

	// 收到任何消息都重置读取超时
	c.Conn.SetReadDeadline(time.Now().Add(180 * time.Second))

	switch msg.Type {
	case WSMsgHeartbeatAck:
		logger.Debug("[WS] 收到心跳响应: %s", c.ID)

	case WSMsgClientReady:
		if threads, ok := msg.Data["max_threads"].(float64); ok && threads > 0 {
			c.MaxThreads = int(threads)
		} else {
			c.MaxThreads = 1
		}
		if ver, ok := msg.Data["client_version"].(string); ok {
			c.ClientVersion = ver
		}
		logger.Info("[WS] 客户端 %s 就绪 (v%s, 线程:%d)", c.ID, c.ClientVersion, c.MaxThreads)
		c.Server.assignTask(c)

	case WSMsgRequestTask:
		logger.Debug("[WS] 客户端 %s 请求任务", c.ID)
		c.Server.assignTask(c)

	case WSMsgRegisterResult:
		// 注册结果
		c.Server.handleRegisterResult(msg.Data)

	case WSMsgRefreshResult:
		// 续期结果
		c.Server.handleRefreshResult(msg.Data)
	}
}

func (ps *PoolServer) removeClient(clientID string) {
	ps.clientsMu.Lock()
	defer ps.clientsMu.Unlock()
	if client, ok := ps.clients[clientID]; ok {
		close(client.Send)
		delete(ps.clients, clientID)
		logger.Info("[WS] 客户端断开: %s (剩余: %d)", clientID, len(ps.clients))
	}
}
func (ps *PoolServer) taskDispatcher() {
	for {
		select {
		case count := <-ps.registerQueue:
			// 分发注册任务（轮询分配）
			ps.assignTaskRoundRobin(WSMsgTaskRegister, map[string]interface{}{
				"count": count,
			})

		case acc := <-ps.refreshQueue:
			// 分发续期任务（轮询分配）
			ps.assignTaskRoundRobin(WSMsgTaskRefresh, map[string]interface{}{
				"email":         acc.Data.Email,
				"cookies":       acc.Data.Cookies,
				"authorization": acc.Data.Authorization,
				"config_id":     acc.ConfigID,
				"csesidx":       acc.CSESIDX,
			})
		}
	}
}

// assignTaskRoundRobin 轮询分配任务给单个客户端
func (ps *PoolServer) assignTaskRoundRobin(msgType WSMessageType, data map[string]interface{}) bool {
	msg := WSMessage{
		Type:      msgType,
		Timestamp: time.Now().Unix(),
		Data:      data,
	}
	msgBytes, _ := json.Marshal(msg)

	ps.clientsMu.Lock()
	defer ps.clientsMu.Unlock()

	if len(ps.clients) == 0 {
		return false
	}

	// 获取客户端列表
	clientList := make([]*WSClient, 0, len(ps.clients))
	for _, client := range ps.clients {
		if client.IsAlive {
			clientList = append(clientList, client)
		}
	}

	if len(clientList) == 0 {
		return false
	}

	// 轮询分配
	ps.nextClientIdx = ps.nextClientIdx % len(clientList)
	client := clientList[ps.nextClientIdx]
	ps.nextClientIdx++

	select {
	case client.Send <- msgBytes:
		logger.Info("[分配] 任务 %s 分配给 %s", msgType, client.ID)
		return true
	default:
		// 发送队列满，尝试下一个
		for i := 0; i < len(clientList)-1; i++ {
			ps.nextClientIdx = ps.nextClientIdx % len(clientList)
			client = clientList[ps.nextClientIdx]
			ps.nextClientIdx++
			select {
			case client.Send <- msgBytes:
				logger.Info("[分配] 任务 %s 分配给 %s", msgType, client.ID)
				return true
			default:
				continue
			}
		}
	}
	return false
}
func (ps *PoolServer) assignTask(client *WSClient) {
	maxThreads := client.MaxThreads
	if maxThreads <= 0 {
		maxThreads = 1
	}
	assignedCount := 0

	// 如果配置了401自动删除，直接删除待续期的401账号，不下发续期任务
	if AutoDelete401 {
		ps.pool.mu.Lock()
		var toDelete []*Account
		var remaining []*Account
		for _, acc := range ps.pool.pendingAccounts {
			if !acc.Refreshed && acc.FailCount > 0 {
				// 401账号，标记删除
				toDelete = append(toDelete, acc)
			} else {
				remaining = append(remaining, acc)
			}
		}
		ps.pool.pendingAccounts = remaining
		ps.pool.mu.Unlock()

		// 删除401账号文件
		for _, acc := range toDelete {
			logger.Info("🗑️ [服务端] 401自动删除账号: %s", acc.Data.Email)
			ps.pool.RemoveAccount(acc)
		}
	} else {
		// 未配置自动删除，分配续期任务给节点
		// 计算401最大重试次数
		maxRetry := MaxFailCount * 3
		if maxRetry < 10 {
			maxRetry = 10
		}

		ps.pool.mu.RLock()
		var refreshAccounts []*Account
		for _, acc := range ps.pool.pendingAccounts {
			if !acc.Refreshed && acc.FailCount > 0 {
				// 跳过已达上限的账号（浏览器刷新已达上限且401失败次数超过阈值）
				if acc.BrowserRefreshCount >= BrowserRefreshMaxRetry && acc.FailCount >= maxRetry {
					continue
				}
				refreshAccounts = append(refreshAccounts, acc)
				if len(refreshAccounts) >= maxThreads {
					break
				}
			}
		}
		ps.pool.mu.RUnlock()
		for _, acc := range refreshAccounts {
			logger.Info("[WS] 分配续期任务给 %s: %s", client.ID, acc.Data.Email)
			msg := WSMessage{
				Type:      WSMsgTaskRefresh,
				Timestamp: time.Now().Unix(),
				Data: map[string]interface{}{
					"email":         acc.Data.Email,
					"cookies":       acc.Data.Cookies,
					"authorization": acc.Data.Authorization,
					"config_id":     acc.ConfigID,
					"csesidx":       acc.CSESIDX,
				},
			}
			msgBytes, _ := json.Marshal(msg)
			select {
			case client.Send <- msgBytes:
				assignedCount++
			default:
			}
		}
	}
	remainingSlots := maxThreads - assignedCount
	if remainingSlots > 0 {
		currentCount := ps.pool.TotalCount()
		targetCount := ps.config.TargetCount
		needCount := targetCount - currentCount

		if needCount > 0 {
			registerCount := remainingSlots
			if registerCount > needCount {
				registerCount = needCount
			}

			logger.Info("[WS] 分配注册任务给 %s: %d个 (当前: %d, 目标: %d, 线程: %d)",
				client.ID, registerCount, currentCount, targetCount, maxThreads)
			for i := 0; i < registerCount; i++ {
				msg := WSMessage{
					Type:      WSMsgTaskRegister,
					Timestamp: time.Now().Unix(),
					Data: map[string]interface{}{
						"count": 1,
					},
				}
				msgBytes, _ := json.Marshal(msg)
				select {
				case client.Send <- msgBytes:
					assignedCount++
				default:
				}
			}
		}
	}

	if assignedCount == 0 {
		logger.Debug("[WS] 无任务需要分配给 %s", client.ID)
	}
}

// heartbeatChecker 心跳检测
func (ps *PoolServer) heartbeatChecker() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		ps.clientsMu.RLock()
		for id, client := range ps.clients {
			client.mu.Lock()
			if time.Since(client.LastPing) > 180*time.Second {
				client.IsAlive = false
				logger.Warn("[WS] 客户端 %s 心跳超时 (last: %v ago)", id, time.Since(client.LastPing))
			}
			client.mu.Unlock()
		}
		ps.clientsMu.RUnlock()
	}
}

func (ps *PoolServer) handleRegisterResult(data map[string]interface{}) {
	success, _ := data["success"].(bool)
	email, _ := data["email"].(string)

	if success {
		logger.Info("✅ 注册成功: %s", email)
		// 重新加载账号
		ps.pool.Load(ps.config.DataDir)
	} else {
		errMsg, _ := data["error"].(string)
		logger.Warn("❌ 注册失败: %s", errMsg)
	}
}
func (ps *PoolServer) handleRefreshResult(data map[string]interface{}) {
	email, _ := data["email"].(string)
	success, _ := data["success"].(bool)

	if success {
		logger.Info("✅ 账号续期成功: %s", email)
		// 更新账号数据
		if cookiesData, ok := data["cookies"]; ok {
			ps.updateAccountCookies(email, cookiesData)
		}
	} else {
		errMsg, _ := data["error"].(string)
		logger.Warn("❌ 账号续期失败 %s: %s", email, errMsg)
		action := ps.config.ExpiredAction
		if action == "" {
			action = "delete" // 默认删除
		}

		switch action {
		case "delete":
			ps.deleteAccount(email)
		case "queue":
			// 保持在队列中，不做处理
		case "refresh":
		default:
			ps.deleteAccount(email)
		}
	}
}

// deleteAccount 删除账号
func (ps *PoolServer) deleteAccount(email string) {
	ps.pool.mu.Lock()
	defer ps.pool.mu.Unlock()

	// 从 pending 队列删除
	for i, acc := range ps.pool.pendingAccounts {
		if acc.Data.Email == email {
			// 删除文件
			if acc.FilePath != "" {
				os.Remove(acc.FilePath)
			}
			ps.pool.pendingAccounts = append(ps.pool.pendingAccounts[:i], ps.pool.pendingAccounts[i+1:]...)
			logger.Info("🗑️ 已删除续期失败账号: %s", email)
			return
		}
	}

	// 从 ready 队列删除
	for i, acc := range ps.pool.readyAccounts {
		if acc.Data.Email == email {
			if acc.FilePath != "" {
				os.Remove(acc.FilePath)
			}
			ps.pool.readyAccounts = append(ps.pool.readyAccounts[:i], ps.pool.readyAccounts[i+1:]...)
			logger.Info("🗑️ 已删除续期失败账号: %s", email)
			return
		}
	}
}

// updateAccountCookies 更新账号Cookie
func (ps *PoolServer) updateAccountCookies(email string, cookiesData interface{}) {
	ps.pool.mu.Lock()
	defer ps.pool.mu.Unlock()

	for _, acc := range ps.pool.readyAccounts {
		if acc.Data.Email == email {
			// 更新cookies
			if cookies, ok := cookiesData.([]interface{}); ok {
				var newCookies []Cookie
				for _, c := range cookies {
					if cm, ok := c.(map[string]interface{}); ok {
						newCookies = append(newCookies, Cookie{
							Name:   cm["name"].(string),
							Value:  cm["value"].(string),
							Domain: cm["domain"].(string),
						})
					}
				}
				acc.Data.Cookies = newCookies
				acc.Refreshed = true
				acc.FailCount = 0
				acc.SaveToFile()
			}
			return
		}
	}
}

// handleQueueRegister 队列注册任务
func (ps *PoolServer) handleQueueRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Count <= 0 {
		req.Count = 1
	}

	select {
	case ps.registerQueue <- req.Count:
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": fmt.Sprintf("已添加 %d 个注册任务到队列", req.Count),
		})
	default:
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "任务队列已满",
		})
	}
}

// handleQueueRefresh 队列续期任务
func (ps *PoolServer) handleQueueRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 查找账号
	ps.pool.mu.RLock()
	var targetAcc *Account
	for _, acc := range ps.pool.readyAccounts {
		if acc.Data.Email == req.Email {
			targetAcc = acc
			break
		}
	}
	ps.pool.mu.RUnlock()

	if targetAcc == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "账号未找到",
		})
		return
	}

	select {
	case ps.refreshQueue <- targetAcc:
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": fmt.Sprintf("已添加账号 %s 续期任务到队列", req.Email),
		})
	default:
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "任务队列已满",
		})
	}
}

// AccountResponse 账号响应
type AccountResponse struct {
	Success       bool   `json:"success"`
	Email         string `json:"email,omitempty"`
	JWT           string `json:"jwt,omitempty"`
	ConfigID      string `json:"config_id,omitempty"`
	Authorization string `json:"authorization,omitempty"`
	Error         string `json:"error,omitempty"`
}

func (ps *PoolServer) handleNext(w http.ResponseWriter, r *http.Request) {
	acc := ps.pool.Next()
	if acc == nil {
		json.NewEncoder(w).Encode(AccountResponse{
			Success: false,
			Error:   "没有可用账号",
		})
		return
	}

	jwt, configID, err := acc.GetJWT()
	if err != nil {
		json.NewEncoder(w).Encode(AccountResponse{
			Success: false,
			Email:   acc.Data.Email,
			Error:   err.Error(),
		})
		return
	}

	json.NewEncoder(w).Encode(AccountResponse{
		Success:       true,
		Email:         acc.Data.Email,
		JWT:           jwt,
		ConfigID:      configID,
		Authorization: acc.Data.Authorization,
	})
}

func (ps *PoolServer) handleMark(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email   string `json:"email"`
		Success bool   `json:"success"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 查找账号并标记
	ps.pool.mu.RLock()
	var targetAcc *Account
	for _, acc := range ps.pool.readyAccounts {
		if acc.Data.Email == req.Email {
			targetAcc = acc
			break
		}
	}
	ps.pool.mu.RUnlock()

	if targetAcc != nil {
		ps.pool.MarkUsed(targetAcc, req.Success)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (ps *PoolServer) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 查找账号并标记需要刷新
	ps.pool.mu.RLock()
	var targetAcc *Account
	for _, acc := range ps.pool.readyAccounts {
		if acc.Data.Email == req.Email {
			targetAcc = acc
			break
		}
	}
	ps.pool.mu.RUnlock()

	if targetAcc != nil {
		ps.pool.MarkNeedsRefresh(targetAcc)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (ps *PoolServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(ps.pool.Stats())
}

func (ps *PoolServer) handleGetJWT(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	if email == "" {
		http.Error(w, "缺少email参数", http.StatusBadRequest)
		return
	}

	ps.pool.mu.RLock()
	var targetAcc *Account
	for _, acc := range ps.pool.readyAccounts {
		if acc.Data.Email == email {
			targetAcc = acc
			break
		}
	}
	ps.pool.mu.RUnlock()

	if targetAcc == nil {
		json.NewEncoder(w).Encode(AccountResponse{
			Success: false,
			Error:   "账号未找到",
		})
		return
	}

	jwt, configID, err := targetAcc.GetJWT()
	if err != nil {
		json.NewEncoder(w).Encode(AccountResponse{
			Success: false,
			Email:   email,
			Error:   err.Error(),
		})
		return
	}

	json.NewEncoder(w).Encode(AccountResponse{
		Success:       true,
		Email:         email,
		JWT:           jwt,
		ConfigID:      configID,
		Authorization: targetAcc.Data.Authorization,
	})
}

// ==================== 远程号池客户端 ====================

// RemotePoolClient 远程号池客户端
type RemotePoolClient struct {
	serverAddr string
	secret     string
	client     *http.Client
	mu         sync.RWMutex
	// 本地缓存
	cachedAccounts map[string]*CachedAccount
}

// CachedAccount 缓存的账号信息
type CachedAccount struct {
	Email         string
	JWT           string
	ConfigID      string
	Authorization string
	FetchedAt     time.Time
}

// NewRemotePoolClient 创建远程号池客户端
func NewRemotePoolClient(serverAddr, secret string) *RemotePoolClient {
	return &RemotePoolClient{
		serverAddr: serverAddr,
		secret:     secret,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		cachedAccounts: make(map[string]*CachedAccount),
	}
}

// doRequest 发送请求到号池服务器
func (rc *RemotePoolClient) doRequest(method, path string, body interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, rc.serverAddr+path, reqBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if rc.secret != "" {
		req.Header.Set("X-Pool-Secret", rc.secret)
	}

	return rc.client.Do(req)
}

// Next 获取下一个可用账号
func (rc *RemotePoolClient) Next() (*CachedAccount, error) {
	resp, err := rc.doRequest("GET", "/pool/next", nil)
	if err != nil {
		return nil, fmt.Errorf("请求号池服务器失败: %w", err)
	}
	defer resp.Body.Close()

	var result AccountResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	if !result.Success {
		return nil, fmt.Errorf("%s", result.Error)
	}

	acc := &CachedAccount{
		Email:         result.Email,
		JWT:           result.JWT,
		ConfigID:      result.ConfigID,
		Authorization: result.Authorization,
		FetchedAt:     time.Now(),
	}

	// 缓存账号
	rc.mu.Lock()
	rc.cachedAccounts[result.Email] = acc
	rc.mu.Unlock()

	return acc, nil
}

// MarkUsed 标记账号使用结果
func (rc *RemotePoolClient) MarkUsed(email string, success bool) error {
	resp, err := rc.doRequest("POST", "/pool/mark", map[string]interface{}{
		"email":   email,
		"success": success,
	})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// MarkNeedsRefresh 标记账号需要刷新
func (rc *RemotePoolClient) MarkNeedsRefresh(email string) error {
	resp, err := rc.doRequest("POST", "/pool/refresh", map[string]interface{}{
		"email": email,
	})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// GetStatus 获取号池状态
func (rc *RemotePoolClient) GetStatus() (map[string]interface{}, error) {
	resp, err := rc.doRequest("GET", "/pool/status", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

// RefreshJWT 刷新指定账号的JWT
func (rc *RemotePoolClient) RefreshJWT(email string) (*CachedAccount, error) {
	resp, err := rc.doRequest("GET", "/pool/jwt?email="+email, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result AccountResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if !result.Success {
		return nil, fmt.Errorf("%s", result.Error)
	}

	acc := &CachedAccount{
		Email:         result.Email,
		JWT:           result.JWT,
		ConfigID:      result.ConfigID,
		Authorization: result.Authorization,
		FetchedAt:     time.Now(),
	}

	rc.mu.Lock()
	rc.cachedAccounts[email] = acc
	rc.mu.Unlock()

	return acc, nil
}

type AccountUploadRequest struct {
	Email         string   `json:"email"`
	FullName      string   `json:"full_name"`
	Cookies       []Cookie `json:"cookies"`
	CookieString  string   `json:"cookie_string"`
	Authorization string   `json:"authorization"`
	ConfigID      string   `json:"config_id"`
	CSESIDX       string   `json:"csesidx"`
	IsNew         bool     `json:"is_new"`
}

// handleUploadAccount 处理账号上传（客户端回传鉴权文件）
func (ps *PoolServer) handleUploadAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AccountUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Error("解析账号上传请求失败: %v", err)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "无效的请求格式",
		})
		return
	}

	if req.Email == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "邮箱不能为空",
		})
		return
	}

	// 构建账号数据
	accData := AccountData{
		Email:         req.Email,
		FullName:      req.FullName,
		Cookies:       req.Cookies,
		CookieString:  req.CookieString,
		Authorization: req.Authorization,
		ConfigID:      req.ConfigID,
		CSESIDX:       req.CSESIDX,
		Timestamp:     time.Now().Format(time.RFC3339),
	}

	// 保存到文件
	dataDir := ps.config.DataDir
	if dataDir == "" {
		dataDir = "./data"
	}

	// 确保目录存在
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		logger.Error("创建数据目录失败: %v", err)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "服务器内部错误",
		})
		return
	}

	// 生成文件名
	filename := fmt.Sprintf("%s.json", req.Email)
	filePath := filepath.Join(dataDir, filename)

	// 序列化并保存
	data, err := json.MarshalIndent(accData, "", "  ")
	if err != nil {
		logger.Error("序列化账号数据失败: %v", err)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "序列化失败",
		})
		return
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		logger.Error("保存账号文件失败: %v", err)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "保存失败",
		})
		return
	}

	if req.IsNew {
		logger.Info("✅ 收到新注册账号: %s", req.Email)
	} else {
		logger.Info("✅ 收到账号续期数据: %s", req.Email)
	}

	// 先加载文件确保账号存在
	ps.pool.Load(dataDir)

	// 更新内存中的账号数据
	ps.pool.mu.Lock()
	found := false

	// 查找并更新 pending 队列
	for i, acc := range ps.pool.pendingAccounts {
		if acc.Data.Email == req.Email {
			acc.Data.Cookies = req.Cookies
			acc.Data.CookieString = req.CookieString
			acc.Data.Authorization = req.Authorization
			acc.Data.ConfigID = req.ConfigID
			acc.Data.CSESIDX = req.CSESIDX
			acc.ConfigID = req.ConfigID
			acc.CSESIDX = req.CSESIDX
			acc.Refreshed = true
			acc.FailCount = 0
			acc.BrowserRefreshCount = 0
			acc.LastRefresh = time.Now()
			acc.JWTExpires = time.Time{}
			// 从 pending 移除
			ps.pool.pendingAccounts = append(ps.pool.pendingAccounts[:i], ps.pool.pendingAccounts[i+1:]...)
			ps.pool.mu.Unlock()
			// 加入 ready 队列
			ps.pool.MarkReady(acc)
			found = true
			goto respond
		}
	}

	// 查找并更新 ready 队列
	for _, acc := range ps.pool.readyAccounts {
		if acc.Data.Email == req.Email {
			acc.Mu.Lock()
			acc.Data.Cookies = req.Cookies
			acc.Data.CookieString = req.CookieString
			acc.Data.Authorization = req.Authorization
			acc.Data.ConfigID = req.ConfigID
			acc.Data.CSESIDX = req.CSESIDX
			acc.ConfigID = req.ConfigID
			acc.CSESIDX = req.CSESIDX
			acc.FailCount = 0
			acc.BrowserRefreshCount = 0
			acc.LastRefresh = time.Now()
			acc.JWTExpires = time.Time{}
			acc.Mu.Unlock()
			found = true
			break
		}
	}
	ps.pool.mu.Unlock()

	if !found {
		logger.Warn("⚠️ [%s] 账号已保存但未在内存中找到", req.Email)
	}

respond:

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("账号 %s 已保存", req.Email),
	})
}

func (rc *RemotePoolClient) UploadAccount(acc *AccountUploadRequest) error {
	data, err := json.Marshal(acc)
	if err != nil {
		return err
	}

	resp, err := rc.doRequest("POST", "/pool/upload-account", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	if success, ok := result["success"].(bool); !ok || !success {
		errMsg, _ := result["error"].(string)
		return fmt.Errorf("上传失败: %s", errMsg)
	}
	return nil
}

type ClientInfo struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Threads  int    `json:"threads"`
	IsAlive  bool   `json:"is_alive"`
	LastPing int64  `json:"last_ping"`
}

func (ps *PoolServer) GetClientsInfo() []ClientInfo {
	ps.clientsMu.RLock()
	defer ps.clientsMu.RUnlock()

	clients := make([]ClientInfo, 0, len(ps.clients))
	for id, c := range ps.clients {
		clients = append(clients, ClientInfo{
			ID:       id,
			Version:  c.ClientVersion,
			Threads:  c.MaxThreads,
			IsAlive:  c.IsAlive,
			LastPing: c.LastPing.Unix(),
		})
	}
	return clients
}
func (ps *PoolServer) GetClientCount() int {
	ps.clientsMu.RLock()
	defer ps.clientsMu.RUnlock()
	return len(ps.clients)
}

func (ps *PoolServer) GetTotalThreads() int {
	ps.clientsMu.RLock()
	defer ps.clientsMu.RUnlock()
	total := 0
	for _, c := range ps.clients {
		total += c.MaxThreads
	}
	return total
}
