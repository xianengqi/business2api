package pool

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

	"business2api/src/logger"
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
	Email           string            `json:"email"`
	FullName        string            `json:"fullName"`
	Authorization   string            `json:"authorization"`
	Cookies         []Cookie          `json:"cookies"`
	CookieString    string            `json:"cookie_string,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	Timestamp       string            `json:"timestamp"`
	ConfigID        string            `json:"configId,omitempty"`
	CSESIDX         string            `json:"csesidx,omitempty"`
}

func ParseCookieString(cookieStr string) []Cookie {
	var cookies []Cookie
	if cookieStr == "" {
		return cookies
	}

	parts := strings.Split(cookieStr, "; ")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, "=")
		if idx > 0 {
			cookies = append(cookies, Cookie{
				Name:   part[:idx],
				Value:  part[idx+1:],
				Domain: ".gemini.google", // 默认域名
			})
		}
	}
	return cookies
}

func (a *AccountData) GetAllCookies() []Cookie {
	if len(a.Cookies) > 0 {
		return a.Cookies
	}
	if a.CookieString != "" {
		return ParseCookieString(a.CookieString)
	}
	return nil
}

// AccountStatus 账号状态
type AccountStatus int

const (
	StatusPending  AccountStatus = iota // 待刷新
	StatusReady                         // 就绪可用
	StatusCooldown                      // 冷却中
	StatusInvalid                       // 失效
)

// Account 账号实例
type Account struct {
	Data                AccountData
	FilePath            string
	JWT                 string
	JWTExpires          time.Time
	ConfigID            string
	CSESIDX             string
	LastRefresh         time.Time
	LastUsed            time.Time // 最后使用时间
	Refreshed           bool
	FailCount           int // 连续失败次数
	BrowserRefreshCount int // 浏览器刷新尝试次数
	SuccessCount        int // 成功次数
	TotalCount          int // 总使用次数
	Status              AccountStatus
	Mu                  sync.Mutex
}

// SetCooldownMultiplier 设置冷却时间倍数（用于429限流）
func (acc *Account) SetCooldownMultiplier(multiplier int) {
	acc.Mu.Lock()
	acc.LastUsed = time.Now().Add(UseCooldown * time.Duration(multiplier-1))
	acc.Mu.Unlock()
}

// 默认冷却时间（可通过配置覆盖）
var (
	RefreshCooldown        = 4 * time.Minute  // 刷新冷却
	UseCooldown            = 15 * time.Second // 使用冷却
	JWTRefreshThreshold    = 60 * time.Second // JWT刷新阈值
	MaxFailCount           = 3                // 最大连续失败次数
	EnableBrowserRefresh   = true             // 是否启用浏览器刷新
	BrowserRefreshHeadless = true             // 浏览器刷新是否无头模式
	BrowserRefreshMaxRetry = 1                // 浏览器刷新最大重试次数
	AutoDelete401          = false            // 401时是否自动删除账号
	DataDir                string
	DefaultConfig          string
	Proxy                  string
	JwtTTL                 = 270 * time.Second
	HTTPClient             *http.Client
)

type RefreshCookieFunc func(acc *Account, headless bool, proxy string) *BrowserRefreshResult
type BrowserRefreshResult struct {
	Success         bool
	SecureCookies   []Cookie
	Authorization   string
	ConfigID        string
	CSESIDX         string
	ResponseHeaders map[string]string
	Error           error
}

var RefreshCookieWithBrowser RefreshCookieFunc

func readResponseBody(resp *http.Response) ([]byte, error) {
	body := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	return body, nil
}

type AccountPool struct {
	readyAccounts   []*Account
	pendingAccounts []*Account
	index           uint64
	mu              sync.RWMutex
	refreshInterval time.Duration
	refreshWorkers  int
	stopChan        chan struct{}
	totalSuccess    int64
	totalFailed     int64
	totalRequests   int64
}

func (p *AccountPool) GetReadyAccounts() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.readyAccounts
}
func (p *AccountPool) GetPendingAccounts() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pendingAccounts
}
func (p *AccountPool) WithLock(fn func(ready, pending []*Account)) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	fn(p.readyAccounts, p.pendingAccounts)
}
func (p *AccountPool) WithWriteLock(fn func(ready, pending []*Account) ([]*Account, []*Account)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readyAccounts, p.pendingAccounts = fn(p.readyAccounts, p.pendingAccounts)
}

var Pool = &AccountPool{
	refreshInterval: 5 * time.Second,
	refreshWorkers:  5,
	stopChan:        make(chan struct{}),
}

func SetCooldowns(refreshSec, useSec int) {
	if refreshSec > 0 {
		RefreshCooldown = time.Duration(refreshSec) * time.Second
	}
	if useSec > 0 {
		UseCooldown = time.Duration(useSec) * time.Second
	}
	logger.Info("⚙️ 冷却配置: 刷新=%v, 使用=%v", RefreshCooldown, UseCooldown)
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

	acc.Mu.Lock()
	acc.Refreshed = false
	acc.Mu.Unlock()

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
	acc.Mu.Lock()
	defer acc.Mu.Unlock()

	acc.Data.Timestamp = time.Now().Format(time.RFC3339)

	// 同时生成 cookie 字符串（方便调试和兼容老版本）
	if len(acc.Data.Cookies) > 0 {
		var cookieParts []string
		for _, c := range acc.Data.Cookies {
			cookieParts = append(cookieParts, fmt.Sprintf("%s=%s", c.Name, c.Value))
		}
		acc.Data.CookieString = strings.Join(cookieParts, "; ")
	}

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

		// 检查冷却
		if time.Since(acc.LastRefresh) < RefreshCooldown {
			acc.Mu.Lock()
			acc.Refreshed = true
			acc.Status = StatusReady
			acc.Mu.Unlock()
			p.MarkReady(acc)
			continue
		}

		acc.JWTExpires = time.Time{}
		if err := acc.RefreshJWT(); err != nil {
			errMsg := err.Error()

			// 认证失败：根据配置决定是否删除或尝试刷新
			if strings.Contains(errMsg, "账号失效") ||
				strings.Contains(errMsg, "401") ||
				strings.Contains(errMsg, "403") {
				log.Printf("⚠️ [worker-%d] [%s] 认证失效: %v", id, acc.Data.Email, err)

				// 如果配置了401自动删除，直接删除账号
				if AutoDelete401 {
					log.Printf("🗑️ [worker-%d] [%s] 401自动删除已启用，移除账号", id, acc.Data.Email)
					acc.Mu.Lock()
					acc.Status = StatusInvalid
					acc.Mu.Unlock()
					p.RemoveAccount(acc)
					continue
				}

				// 检查是否可以进行浏览器刷新
				acc.Mu.Lock()
				browserRefreshCount := acc.BrowserRefreshCount
				acc.Mu.Unlock()

				// 尝试浏览器刷新（有次数限制）
				if EnableBrowserRefresh && BrowserRefreshMaxRetry > 0 && browserRefreshCount < BrowserRefreshMaxRetry {
					acc.Mu.Lock()
					acc.BrowserRefreshCount++
					acc.Mu.Unlock()
					refreshResult := RefreshCookieWithBrowser(acc, BrowserRefreshHeadless, Proxy)

					if refreshResult.Success {
						acc.Mu.Lock()
						acc.Data.Cookies = refreshResult.SecureCookies
						if refreshResult.Authorization != "" {
							acc.Data.Authorization = refreshResult.Authorization
						}
						if refreshResult.ConfigID != "" {
							acc.ConfigID = refreshResult.ConfigID
							acc.Data.ConfigID = refreshResult.ConfigID
						}
						if refreshResult.CSESIDX != "" {
							acc.CSESIDX = refreshResult.CSESIDX
							acc.Data.CSESIDX = refreshResult.CSESIDX
						}
						if len(refreshResult.ResponseHeaders) > 0 {
							acc.Data.ResponseHeaders = refreshResult.ResponseHeaders
						}
						acc.FailCount = 0
						acc.BrowserRefreshCount = 0  // 成功后重置计数
						acc.JWTExpires = time.Time{} // 重置JWT过期时间
						acc.Status = StatusPending
						acc.Mu.Unlock()

						// 保存更新后的账号
						if err := acc.SaveToFile(); err != nil {
							log.Printf("⚠️ [%s] 保存刷新后的账号失败: %v", acc.Data.Email, err)
						}
						p.mu.Lock()
						p.pendingAccounts = append(p.pendingAccounts, acc)
						p.mu.Unlock()
						continue
					} else {
						log.Printf("⚠️ [worker-%d] [%s] 浏览器刷新失败: %v", id, acc.Data.Email, refreshResult.Error)
					}
				} else if browserRefreshCount >= BrowserRefreshMaxRetry && BrowserRefreshMaxRetry > 0 {
					log.Printf("⚠️ [worker-%d] [%s] 已达浏览器刷新上限 (%d次)，跳过浏览器刷新", id, acc.Data.Email, BrowserRefreshMaxRetry)
				}
				acc.Mu.Lock()
				acc.FailCount++
				failCount := acc.FailCount
				acc.Mu.Unlock()

				waitTime := time.Duration(failCount*30) * time.Second
				if waitTime > 5*time.Minute {
					waitTime = 5 * time.Minute // 最大等待5分钟
				}
				log.Printf("⏳ [worker-%d] [%s] 401刷新失败 (%d次)，%v后重试", id, acc.Data.Email, failCount, waitTime)
				time.Sleep(waitTime)

				p.mu.Lock()
				p.pendingAccounts = append(p.pendingAccounts, acc)
				p.mu.Unlock()
				continue
			}

			// 冷却中：直接标记就绪
			if strings.Contains(errMsg, "刷新冷却中") {
				acc.Mu.Lock()
				acc.Refreshed = true
				acc.Status = StatusReady
				acc.Mu.Unlock()
				p.MarkReady(acc)
				continue
			}

			// 其他错误：累计失败次数
			acc.Mu.Lock()
			acc.FailCount++
			failCount := acc.FailCount
			acc.Mu.Unlock()

			if failCount >= MaxFailCount {
				log.Printf("❌ [worker-%d] [%s] 连续失败 %d 次，移除账号: %v", id, acc.Data.Email, failCount, err)
				acc.Mu.Lock()
				acc.Status = StatusInvalid
				acc.Mu.Unlock()
				p.RemoveAccount(acc)
			} else {
				log.Printf("⚠️ [worker-%d] [%s] 刷新失败 (%d/%d): %v", id, acc.Data.Email, failCount, MaxFailCount, err)
				// 延迟后重试
				time.Sleep(time.Duration(failCount) * 5 * time.Second)
				p.mu.Lock()
				p.pendingAccounts = append(p.pendingAccounts, acc)
				p.mu.Unlock()
			}
		} else {
			// 刷新成功：重置失败计数
			acc.Mu.Lock()
			acc.FailCount = 0
			acc.Status = StatusReady
			acc.Mu.Unlock()

			if err := acc.SaveToFile(); err != nil {
				log.Printf("⚠️ [%s] 写回文件失败: %v", acc.Data.Email, err)
			}
			p.MarkReady(acc)
		}
	}
}

func (p *AccountPool) scanWorker() {
	ticker := time.NewTicker(p.refreshInterval)
	fileScanTicker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	defer fileScanTicker.Stop()

	for {
		select {
		case <-p.stopChan:
			return
		case <-fileScanTicker.C:
			p.Load(DataDir)
		case <-ticker.C:
			p.RefreshExpiredAccounts()
		}
	}
}

// RefreshExpiredAccounts 刷新即将过期的账号
func (p *AccountPool) RefreshExpiredAccounts() {
	p.mu.Lock()
	defer p.mu.Unlock()

	var stillReady []*Account
	refreshed := 0
	now := time.Now()

	for _, acc := range p.readyAccounts {
		acc.Mu.Lock()
		jwtExpires := acc.JWTExpires
		lastRefresh := acc.LastRefresh
		acc.Mu.Unlock()

		needsRefresh := jwtExpires.IsZero() || now.Add(JWTRefreshThreshold).After(jwtExpires)
		inCooldown := now.Sub(lastRefresh) < RefreshCooldown

		if needsRefresh && !inCooldown {
			acc.Mu.Lock()
			acc.Refreshed = false
			acc.Mu.Unlock()
			p.pendingAccounts = append(p.pendingAccounts, acc)
			refreshed++
		} else {
			stillReady = append(stillReady, acc)
		}
	}

	p.readyAccounts = stillReady
	if refreshed > 0 {
		log.Printf("🔄 扫描刷新: %d 个账号JWT即将过期", refreshed)
	}
}

func (p *AccountPool) RefreshAllAccounts() {
	p.mu.Lock()
	defer p.mu.Unlock()

	var stillReady []*Account
	refreshed, skipped := 0, 0

	for _, acc := range p.readyAccounts {
		if time.Since(acc.LastRefresh) < RefreshCooldown {
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
	if refreshed > 0 {
		log.Printf("🔄 全量刷新: %d 个账号已加入刷新队列，%d 个在冷却中跳过", refreshed, skipped)
	}
}

func (p *AccountPool) Next() *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.readyAccounts) == 0 {
		return nil
	}

	n := len(p.readyAccounts)
	startIdx := atomic.AddUint64(&p.index, 1) - 1
	now := time.Now()

	var bestAccount *Account
	var oldestUsed time.Time

	// 第一轮：找不在使用冷却中的账号
	for i := 0; i < n; i++ {
		acc := p.readyAccounts[(startIdx+uint64(i))%uint64(n)]
		acc.Mu.Lock()
		inUseCooldown := now.Sub(acc.LastUsed) < UseCooldown
		lastUsed := acc.LastUsed
		acc.Mu.Unlock()

		if !inUseCooldown {
			// 找到可用账号，标记使用时间
			acc.Mu.Lock()
			acc.LastUsed = now
			acc.TotalCount++
			acc.Mu.Unlock()
			atomic.AddInt64(&p.totalRequests, 1)
			return acc
		}

		// 记录最久未使用的账号作为备选
		if bestAccount == nil || lastUsed.Before(oldestUsed) {
			bestAccount = acc
			oldestUsed = lastUsed
		}
	}

	// 所有账号都在冷却中，返回最久未使用的
	if bestAccount != nil {
		bestAccount.Mu.Lock()
		bestAccount.LastUsed = now
		bestAccount.TotalCount++
		bestAccount.Mu.Unlock()
		atomic.AddInt64(&p.totalRequests, 1)
		log.Printf("⏳ 所有账号在使用冷却中，选择最久未用: %s", bestAccount.Data.Email)
	}
	return bestAccount
}

// MarkUsed 标记账号已使用（成功）
func (p *AccountPool) MarkUsed(acc *Account, success bool) {
	if acc == nil {
		return
	}
	acc.Mu.Lock()
	defer acc.Mu.Unlock()

	if success {
		acc.SuccessCount++
		acc.FailCount = 0 // 重置连续失败
		atomic.AddInt64(&p.totalSuccess, 1)
	} else {
		acc.FailCount++
		atomic.AddInt64(&p.totalFailed, 1)
	}
}

// MarkNeedsRefresh 标记账号需要刷新（遇到401/403等）
func (p *AccountPool) MarkNeedsRefresh(acc *Account) {
	if acc == nil {
		return
	}
	acc.Mu.Lock()
	acc.LastRefresh = time.Time{} // 重置刷新时间，强制刷新
	acc.Mu.Unlock()
	p.MarkPending(acc)
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

// Stats 返回号池统计信息
func (p *AccountPool) Stats() map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()

	totalSuccess := atomic.LoadInt64(&p.totalSuccess)
	totalFailed := atomic.LoadInt64(&p.totalFailed)
	totalRequests := atomic.LoadInt64(&p.totalRequests)

	successRate := float64(0)
	if totalRequests > 0 {
		successRate = float64(totalSuccess) / float64(totalRequests) * 100
	}

	return map[string]interface{}{
		"ready":          len(p.readyAccounts),
		"pending":        len(p.pendingAccounts),
		"total":          len(p.readyAccounts) + len(p.pendingAccounts),
		"total_requests": totalRequests,
		"total_success":  totalSuccess,
		"total_failed":   totalFailed,
		"success_rate":   fmt.Sprintf("%.1f%%", successRate),
		"cooldowns": map[string]interface{}{
			"refresh_sec": int(RefreshCooldown.Seconds()),
			"use_sec":     int(UseCooldown.Seconds()),
		},
	}
}

// AccountInfo 账号信息（用于API返回）
type AccountInfo struct {
	Email        string    `json:"email"`
	Status       string    `json:"status"`
	LastRefresh  time.Time `json:"last_refresh"`
	LastUsed     time.Time `json:"last_used"`
	FailCount    int       `json:"fail_count"`
	SuccessCount int       `json:"success_count"`
	TotalCount   int       `json:"total_count"`
	JWTExpires   time.Time `json:"jwt_expires"`
}

// ListAccounts 列出所有账号信息
func (p *AccountPool) ListAccounts() []AccountInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var accounts []AccountInfo
	statusNames := map[AccountStatus]string{
		StatusPending:  "pending",
		StatusReady:    "ready",
		StatusCooldown: "cooldown",
		StatusInvalid:  "invalid",
	}

	addAccounts := func(list []*Account) {
		for _, acc := range list {
			acc.Mu.Lock()
			info := AccountInfo{
				Email:        acc.Data.Email,
				Status:       statusNames[acc.Status],
				LastRefresh:  acc.LastRefresh,
				LastUsed:     acc.LastUsed,
				FailCount:    acc.FailCount,
				SuccessCount: acc.SuccessCount,
				TotalCount:   acc.TotalCount,
				JWTExpires:   acc.JWTExpires,
			}
			acc.Mu.Unlock()
			accounts = append(accounts, info)
		}
	}

	addAccounts(p.readyAccounts)
	addAccounts(p.pendingAccounts)

	return accounts
}

// ForceRefreshAll 强制刷新所有账号
func (p *AccountPool) ForceRefreshAll() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	count := 0
	for _, acc := range p.readyAccounts {
		acc.Mu.Lock()
		acc.Refreshed = false
		acc.JWTExpires = time.Time{}
		acc.LastRefresh = time.Time{} // 强制跳过冷却
		acc.Mu.Unlock()
		p.pendingAccounts = append(p.pendingAccounts, acc)
		count++
	}
	p.readyAccounts = nil

	log.Printf("🔄 强制刷新: %d 个账号已加入刷新队列", count)
	return count
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
	acc.Mu.Lock()
	defer acc.Mu.Unlock()

	// 检查JWT是否仍有效
	if time.Now().Before(acc.JWTExpires) {
		return nil
	}

	// 检查刷新冷却
	if time.Since(acc.LastRefresh) < RefreshCooldown {
		return fmt.Errorf("刷新冷却中，剩余 %.0f 秒", (RefreshCooldown - time.Since(acc.LastRefresh)).Seconds())
	}

	// 获取必要的Cookie
	secureSES := acc.getCookie("__Secure-C_SES")
	hostOSES := acc.getCookie("__Host-C_OSES")

	// 验证Cookie是否存在
	if secureSES == "" {
		return fmt.Errorf("账号失效: 缺少 __Secure-C_SES Cookie")
	}

	// 构建Cookie字符串
	cookie := fmt.Sprintf("__Secure-C_SES=%s", secureSES)
	if hostOSES != "" {
		cookie += fmt.Sprintf("; __Host-C_OSES=%s", hostOSES)
	}

	// 添加其他可能需要的Cookie
	for _, c := range acc.Data.Cookies {
		if c.Name != "__Secure-C_SES" && c.Name != "__Host-C_OSES" {
			if strings.HasPrefix(c.Name, "__Secure-") || strings.HasPrefix(c.Name, "__Host-") {
				cookie += fmt.Sprintf("; %s=%s", c.Name, c.Value)
			}
		}
	}

	req, _ := http.NewRequest("GET", "https://business.gemini.google/auth/getoxsrf", nil)
	q := req.URL.Query()
	q.Add("csesidx", acc.CSESIDX)
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://business.gemini.google/")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("getoxsrf 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := readResponseBody(resp)
		bodyStr := string(body)
		if len(bodyStr) > 200 {
			bodyStr = bodyStr[:200]
		}

		// 详细的错误分类
		switch resp.StatusCode {
		case 401, 403:
			// 认证失败，Cookie可能过期
			return fmt.Errorf("账号失效: %d - Cookie可能已过期", resp.StatusCode)
		case 429:
			// 速率限制
			return fmt.Errorf("请求频率过高: %d", resp.StatusCode)
		case 500, 502, 503, 504:
			// 服务器错误，可能是临时的
			return fmt.Errorf("服务器错误: %d, 稍后重试", resp.StatusCode)
		default:
			return fmt.Errorf("getoxsrf 失败: %d %s", resp.StatusCode, bodyStr)
		}
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
	acc.Mu.Lock()
	defer acc.Mu.Unlock()
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
