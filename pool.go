package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ==================== 数据结构 ====================

// Cookie 账号Cookie
type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

// AccountData 账号数据
type AccountData struct {
	Email         string   `json:"email"`
	FullName      string   `json:"fullName"`
	Authorization string   `json:"authorization"`
	Cookies       []Cookie `json:"cookies"`
	Timestamp     string   `json:"timestamp"`
	ConfigID      string   `json:"configId,omitempty"`
	CSESIDX       string   `json:"csesidx,omitempty"`
}

// Account 账号实例
type Account struct {
	Data        AccountData
	FilePath    string
	JWT         string
	JWTExpires  time.Time
	ConfigID    string
	CSESIDX     string
	LastRefresh time.Time
	LastUsed    time.Time // 最后使用时间
	Refreshed   bool
	mu          sync.Mutex
}

const (
	refreshCooldown     = 4 * time.Minute
	idleRefreshInterval = 5 * time.Hour // 5小时未使用或未刷新则刷新
)

type AccountPool struct {
	readyAccounts   []*Account
	pendingAccounts []*Account
	index           uint64
	mu              sync.RWMutex
	refreshInterval time.Duration
	refreshWorkers  int
	stopChan        chan struct{}
}

var pool = &AccountPool{
	refreshInterval: 5 * time.Second,
	refreshWorkers:  5,
	stopChan:        make(chan struct{}),
}

func (p *AccountPool) Load(dir string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}

	existingAccounts := make(map[string]*Account)
	for _, acc := range p.readyAccounts {
		existingAccounts[acc.FilePath] = acc
	}
	for _, acc := range p.pendingAccounts {
		existingAccounts[acc.FilePath] = acc
	}

	var newReadyAccounts []*Account
	var newPendingAccounts []*Account

	for _, f := range files {
		if acc, ok := existingAccounts[f]; ok {
			if acc.Refreshed {
				newReadyAccounts = append(newReadyAccounts, acc)
			} else {
				newPendingAccounts = append(newPendingAccounts, acc)
			}
			delete(existingAccounts, f)
			continue
		}

		data, err := os.ReadFile(f)
		if err != nil {
			log.Printf("⚠️ 读取 %s 失败: %v", f, err)
			continue
		}

		var acc AccountData
		if err := json.Unmarshal(data, &acc); err != nil {
			log.Printf("⚠️ 解析 %s 失败: %v", f, err)
			continue
		}

		csesidx := acc.CSESIDX
		if csesidx == "" {
			csesidx = extractCSESIDX(acc.Authorization)
		}
		if csesidx == "" {
			log.Printf("⚠️ %s 无法获取 csesidx", f)
			continue
		}

		configID := acc.ConfigID
		if configID == "" && DefaultConfig != "" {
			configID = DefaultConfig
		}

		newPendingAccounts = append(newPendingAccounts, &Account{
			Data:      acc,
			FilePath:  f,
			CSESIDX:   csesidx,
			ConfigID:  configID,
			Refreshed: false,
		})
	}

	p.readyAccounts = newReadyAccounts
	p.pendingAccounts = newPendingAccounts
	return nil
}

// GetPendingAccount 获取待刷新账号
func (p *AccountPool) GetPendingAccount() *Account {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.pendingAccounts) == 0 {
		return nil
	}

	acc := p.pendingAccounts[0]
	p.pendingAccounts = p.pendingAccounts[1:]
	return acc
}

// MarkReady 标记账号为就绪
func (p *AccountPool) MarkReady(acc *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	acc.Refreshed = true
	p.readyAccounts = append(p.readyAccounts, acc)
}

// MarkPending 标记账号待刷新
func (p *AccountPool) MarkPending(acc *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, a := range p.readyAccounts {
		if a == acc {
			p.readyAccounts = append(p.readyAccounts[:i], p.readyAccounts[i+1:]...)
			break
		}
	}

	acc.mu.Lock()
	acc.Refreshed = false
	acc.mu.Unlock()

	p.pendingAccounts = append(p.pendingAccounts, acc)
	log.Printf("🔄 账号 %s 移至刷新池", filepath.Base(acc.FilePath))
}

// RemoveAccount 删除失效账号
func (p *AccountPool) RemoveAccount(acc *Account) {
	if err := os.Remove(acc.FilePath); err != nil {
		log.Printf("⚠️ 删除文件失败 %s: %v", acc.FilePath, err)
	} else {
		log.Printf("🗑️ 已删除失效账号: %s", filepath.Base(acc.FilePath))
	}
}

// SaveToFile 保存账号到文件
func (acc *Account) SaveToFile() error {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	acc.Data.Timestamp = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(acc.Data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化账号数据失败: %w", err)
	}

	if err := os.WriteFile(acc.FilePath, data, 0644); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	return nil
}

// StartPoolManager 启动号池管理器
func (p *AccountPool) StartPoolManager() {
	for i := 0; i < p.refreshWorkers; i++ {
		go p.refreshWorker(i)
	}
	go p.scanWorker()
}

func (p *AccountPool) refreshWorker(id int) {
	for {
		select {
		case <-p.stopChan:
			return
		default:
		}

		acc := p.GetPendingAccount()
		if acc == nil {
			time.Sleep(time.Second)
			continue
		}

		if time.Since(acc.LastRefresh) < refreshCooldown {
			acc.Refreshed = true
			p.MarkReady(acc)
			continue
		}

		acc.JWTExpires = time.Time{}
		if err := acc.RefreshJWT(); err != nil {
			if strings.Contains(err.Error(), "账号失效") {
				log.Printf("❌ [worker-%d] [%s] %v", id, acc.Data.Email, err)
				p.RemoveAccount(acc)
			} else if strings.Contains(err.Error(), "刷新冷却中") {
				acc.Refreshed = true
				p.MarkReady(acc)
			} else {
				log.Printf("⚠️ [worker-%d] [%s] 刷新失败: %v，稍后重试", id, acc.Data.Email, err)
				p.MarkPending(acc)
			}
		} else {
			if err := acc.SaveToFile(); err != nil {
				log.Printf("⚠️ [%s] 写回文件失败: %v", acc.Data.Email, err)
			}
			p.MarkReady(acc)
		}
	}
}

func (p *AccountPool) scanWorker() {
	fileScanTicker := time.NewTicker(5 * time.Minute)
	idleRefreshTicker := time.NewTicker(30 * time.Minute) // 每30分钟检查一次未使用账号
	defer fileScanTicker.Stop()
	defer idleRefreshTicker.Stop()

	for {
		select {
		case <-p.stopChan:
			return
		case <-fileScanTicker.C:
			p.Load(DataDir)
		case <-idleRefreshTicker.C:
			p.RefreshIdleAccounts()
		}
	}
}

// RefreshIdleAccounts 刷新5小时未使用或未刷新的账号
func (p *AccountPool) RefreshIdleAccounts() {
	p.mu.Lock()
	defer p.mu.Unlock()

	var stillReady []*Account
	refreshed := 0
	now := time.Now()

	for _, acc := range p.readyAccounts {
		acc.mu.Lock()
		lastRefresh := acc.LastRefresh
		lastUsed := acc.LastUsed
		acc.mu.Unlock()

		// 5小时未使用或未刷新
		idleSinceRefresh := now.Sub(lastRefresh) >= idleRefreshInterval
		idleSinceUse := lastUsed.IsZero() || now.Sub(lastUsed) >= idleRefreshInterval
		inCooldown := now.Sub(lastRefresh) < refreshCooldown

		if (idleSinceRefresh || idleSinceUse) && !inCooldown {
			acc.mu.Lock()
			acc.Refreshed = false
			acc.mu.Unlock()
			p.pendingAccounts = append(p.pendingAccounts, acc)
			refreshed++
		} else {
			stillReady = append(stillReady, acc)
		}
	}

	p.readyAccounts = stillReady
	if refreshed > 0 {
		log.Printf("🔄 空闲刷新: %d 个账号超过5小时未使用或未刷新", refreshed)
	}
}

func (p *AccountPool) RefreshAllAccounts() {
	p.mu.Lock()
	defer p.mu.Unlock()

	var stillReady []*Account
	refreshed, skipped := 0, 0

	for _, acc := range p.readyAccounts {
		if time.Since(acc.LastRefresh) < refreshCooldown {
			stillReady = append(stillReady, acc)
			skipped++
			continue
		}
		acc.Refreshed = false
		acc.JWTExpires = time.Time{}
		p.pendingAccounts = append(p.pendingAccounts, acc)
		refreshed++
	}

	p.readyAccounts = stillReady
	if refreshed > 0 || skipped > 0 {
	}
}

func (p *AccountPool) Next() *Account {
	p.mu.RLock()
	if len(p.readyAccounts) == 0 {
		p.mu.RUnlock()
		return nil
	}

	n := len(p.readyAccounts)
	startIdx := atomic.AddUint64(&p.index, 1) - 1
	var selectedAcc *Account
	for i := 0; i < n; i++ {
		acc := p.readyAccounts[(startIdx+uint64(i))%uint64(n)]
		acc.mu.Lock()
		inCooldown := time.Since(acc.LastRefresh) < refreshCooldown
		acc.mu.Unlock()
		if !inCooldown {
			selectedAcc = acc
			break
		}
	}
	if selectedAcc == nil {
		selectedAcc = p.readyAccounts[startIdx%uint64(n)]
	}
	p.mu.RUnlock()

	// 取出账号时立即刷新
	if selectedAcc != nil {
		selectedAcc.mu.Lock()
		selectedAcc.LastUsed = time.Now()
		selectedAcc.mu.Unlock()

		// 异步刷新JWT（不阻塞返回）
		go func(acc *Account) {
			acc.mu.Lock()
			needsRefresh := time.Since(acc.LastRefresh) >= refreshCooldown
			acc.mu.Unlock()

			if needsRefresh {
				if err := acc.RefreshJWT(); err != nil {
					if !strings.Contains(err.Error(), "刷新冷却中") {
						log.Printf("⚠️ [%s] 使用时刷新失败: %v", acc.Data.Email, err)
					}
				} else {
					if err := acc.SaveToFile(); err != nil {
						log.Printf("⚠️ [%s] 写回文件失败: %v", acc.Data.Email, err)
					} else {
						log.Printf("✅ [%s] 使用时刷新成功，已写回文件", acc.Data.Email)
					}
				}
			}
		}(selectedAcc)
	}

	return selectedAcc
}

func (p *AccountPool) Count() int { p.mu.RLock(); defer p.mu.RUnlock(); return len(p.readyAccounts) }
func (p *AccountPool) PendingCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.pendingAccounts)
}
func (p *AccountPool) ReadyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.readyAccounts)
}
func (p *AccountPool) TotalCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.readyAccounts) + len(p.pendingAccounts)
}

// MarkNeedsRefresh 标记账号需要刷新（遇到 401 等认证错误时调用）
func (p *AccountPool) MarkNeedsRefresh(acc *Account) {
	if acc == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 从 ready 列表移除
	for i, a := range p.readyAccounts {
		if a == acc {
			p.readyAccounts = append(p.readyAccounts[:i], p.readyAccounts[i+1:]...)
			break
		}
	}

	// 标记需要刷新并加入 pending
	acc.mu.Lock()
	acc.Refreshed = false
	acc.JWTExpires = time.Time{}
	acc.mu.Unlock()

	// 检查是否已在 pending 中
	for _, a := range p.pendingAccounts {
		if a == acc {
			return
		}
	}
	p.pendingAccounts = append(p.pendingAccounts, acc)
	log.Printf("⚠️ [%s] 已标记为需要刷新", acc.Data.Email)
}

func urlsafeB64Encode(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

func kqEncode(s string) string {
	var b []byte
	for _, ch := range s {
		v := int(ch)
		if v > 255 {
			b = append(b, byte(v&255), byte(v>>8))
		} else {
			b = append(b, byte(v))
		}
	}
	return urlsafeB64Encode(b)
}

func createJWT(keyBytes []byte, keyID, csesidx string) string {
	now := time.Now().Unix()
	header := map[string]interface{}{"alg": "HS256", "typ": "JWT", "kid": keyID}
	payload := map[string]interface{}{
		"iss": "https://business.gemini.google",
		"aud": "https://biz-discoveryengine.googleapis.com",
		"sub": fmt.Sprintf("csesidx/%s", csesidx),
		"iat": now, "exp": now + 300, "nbf": now,
	}

	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(payload)
	message := kqEncode(string(headerJSON)) + "." + kqEncode(string(payloadJSON))

	h := hmac.New(sha256.New, keyBytes)
	h.Write([]byte(message))
	return message + "." + urlsafeB64Encode(h.Sum(nil))
}

func extractCSESIDX(auth string) string {
	parts := strings.Split(auth, " ")
	if len(parts) != 2 {
		return ""
	}
	jwtParts := strings.Split(parts[1], ".")
	if len(jwtParts) != 3 {
		return ""
	}

	payload, err := base64.RawURLEncoding.DecodeString(jwtParts[1])
	if err != nil {
		return ""
	}

	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}

	if strings.HasPrefix(claims.Sub, "csesidx/") {
		return strings.TrimPrefix(claims.Sub, "csesidx/")
	}
	return ""
}

// ==================== 账号操作 ====================

func (acc *Account) getCookie(name string) string {
	for _, c := range acc.Data.Cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// RefreshJWT 刷新JWT
func (acc *Account) RefreshJWT() error {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	if time.Now().Before(acc.JWTExpires) {
		return nil
	}

	if time.Since(acc.LastRefresh) < refreshCooldown {
		return fmt.Errorf("刷新冷却中，剩余 %.0f 秒", (refreshCooldown - time.Since(acc.LastRefresh)).Seconds())
	}

	secureSES := acc.getCookie("__Secure-C_SES")
	hostOSES := acc.getCookie("__Host-C_OSES")

	cookie := fmt.Sprintf("__Secure-C_SES=%s", secureSES)
	if hostOSES != "" {
		cookie += fmt.Sprintf("; __Host-C_OSES=%s", hostOSES)
	}

	req, _ := http.NewRequest("GET", "https://business.gemini.google/auth/getoxsrf", nil)
	q := req.URL.Query()
	q.Add("csesidx", acc.CSESIDX)
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://business.gemini.google/")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("getoxsrf 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := readResponseBody(resp)
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("账号失效: %d %s", resp.StatusCode, string(body))
		}
		return fmt.Errorf("getoxsrf 失败: %d %s", resp.StatusCode, string(body))
	}

	body, _ := readResponseBody(resp)
	txt := strings.TrimPrefix(string(body), ")]}'")
	txt = strings.TrimSpace(txt)

	var data struct {
		XsrfToken string `json:"xsrfToken"`
		KeyID     string `json:"keyId"`
	}
	if err := json.Unmarshal([]byte(txt), &data); err != nil {
		return fmt.Errorf("解析 xsrf 响应失败: %w", err)
	}

	token := data.XsrfToken
	switch len(token) % 4 {
	case 2:
		token += "=="
	case 3:
		token += "="
	}
	keyBytes, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return fmt.Errorf("解码 xsrfToken 失败: %w", err)
	}

	acc.JWT = createJWT(keyBytes, data.KeyID, acc.CSESIDX)
	acc.JWTExpires = time.Now().Add(JwtTTL)
	acc.LastRefresh = time.Now()

	if acc.ConfigID == "" {
		configID, err := acc.fetchConfigID()
		if err != nil {
			return fmt.Errorf("获取 configId 失败: %w", err)
		}
		acc.ConfigID = configID
	}
	return nil
}

// GetJWT 获取JWT
func (acc *Account) GetJWT() (string, string, error) {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if acc.JWT == "" {
		return "", "", fmt.Errorf("JWT 为空，账号未刷新")
	}
	return acc.JWT, acc.ConfigID, nil
}

func (acc *Account) fetchConfigID() (string, error) {
	if acc.Data.ConfigID != "" {
		return acc.Data.ConfigID, nil
	}
	if DefaultConfig != "" {
		return DefaultConfig, nil
	}
	return "", fmt.Errorf("未配置 configId")
}
