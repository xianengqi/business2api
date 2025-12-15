package register

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"business2api/src/logger"
	"business2api/src/pool"
)

// ==================== 注册与刷新 ====================

var (
	DataDir       string
	TargetCount   int
	MinCount      int
	CheckInterval time.Duration
	Threads       int
	Headless      bool   // 注册无头模式
	Proxy         string // 代理
)

var IsRegistering int32
var registeringTarget int32 // 正在注册的目标数量
var registerMu sync.Mutex   // 注册启动互斥锁

var Stats = &RegisterStats{}

type RegisterStats struct {
	Total     int       `json:"total"`
	Success   int       `json:"success"`
	Failed    int       `json:"failed"`
	LastError string    `json:"lastError"`
	UpdatedAt time.Time `json:"updatedAt"`
	mu        sync.RWMutex
}

func (s *RegisterStats) AddSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Total++
	s.Success++
	s.UpdatedAt = time.Now()
}

func (s *RegisterStats) AddFailed(err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Total++
	s.Failed++
	s.LastError = err
	s.UpdatedAt = time.Now()
}

func (s *RegisterStats) Get() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]interface{}{
		"total":      s.Total,
		"success":    s.Success,
		"failed":     s.Failed,
		"last_error": s.LastError,
		"updated_at": s.UpdatedAt,
	}
}

// 注册结果
type RegisterResult struct {
	Success  bool   `json:"success"`
	Email    string `json:"email"`
	Error    string `json:"error"`
	NeedWait bool   `json:"needWait"`
}

// StartRegister 启动注册任务（优化并发控制）
func StartRegister(count int) error {
	registerMu.Lock()
	defer registerMu.Unlock()

	// 再次检查当前账号数是否已满足
	pool.Pool.Load(DataDir)
	currentCount := pool.Pool.TotalCount()
	if currentCount >= TargetCount {
		logger.Info("✅ 账号数已满足: %d >= %d，无需注册", currentCount, TargetCount)
		return nil
	}

	// 如果已经在注册中，检查是否需要调整
	if atomic.LoadInt32(&IsRegistering) == 1 {
		currentTarget := atomic.LoadInt32(&registeringTarget)
		if int(currentTarget) >= count {
			return fmt.Errorf("注册进程已在运行，目标: %d", currentTarget)
		}
		// 更新目标数量
		atomic.StoreInt32(&registeringTarget, int32(count))
		logger.Info("🔄 注册目标已更新: %d", count)
		return nil
	}

	if !atomic.CompareAndSwapInt32(&IsRegistering, 0, 1) {
		return fmt.Errorf("注册进程已在运行")
	}
	atomic.StoreInt32(&registeringTarget, int32(count))

	// 获取数据目录的绝对路径
	dataDirAbs, _ := filepath.Abs(DataDir)
	if err := os.MkdirAll(dataDirAbs, 0755); err != nil {
		atomic.StoreInt32(&IsRegistering, 0)
		atomic.StoreInt32(&registeringTarget, 0)
		return fmt.Errorf("创建数据目录失败: %w", err)
	}

	// 使用配置的线程数
	threads := Threads
	if threads <= 0 {
		threads = 1
	}
	for i := 0; i < threads; i++ {
		go NativeRegisterWorker(i+1, dataDirAbs)
	}

	// 监控进度
	go func() {
		for {
			time.Sleep(10 * time.Second)
			pool.Pool.Load(DataDir)
			currentCount := pool.Pool.TotalCount()
			target := atomic.LoadInt32(&registeringTarget)

			// 检查是否达到目标（使用当前目标和全局目标的较大值）
			effectiveTarget := TargetCount
			if int(target) > effectiveTarget {
				effectiveTarget = int(target)
			}

			if currentCount >= effectiveTarget {
				logger.Info("✅ 已达到目标账号数: %d >= %d，停止注册", currentCount, effectiveTarget)
				atomic.StoreInt32(&IsRegistering, 0)
				atomic.StoreInt32(&registeringTarget, 0)
				return
			}
		}
	}()

	return nil
}

// PoolMaintainer 号池维护器
func PoolMaintainer() {
	interval := CheckInterval
	if interval < time.Minute {
		interval = 30 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	CheckAndMaintainPool()

	for range ticker.C {
		CheckAndMaintainPool()
	}
}

// CheckAndMaintainPool 检查并维护号池（优化并发控制）
func CheckAndMaintainPool() {
	// 如果正在注册中，跳过检查
	if atomic.LoadInt32(&IsRegistering) == 1 {
		logger.Debug("⏳ 注册进程运行中，跳过本次检查")
		return
	}

	pool.Pool.Load(DataDir)

	readyCount := pool.Pool.ReadyCount()
	pendingCount := pool.Pool.PendingCount()
	totalCount := pool.Pool.TotalCount()

	logger.Info("📊 号池检查: ready=%d, pending=%d, total=%d, 目标=%d, 最小=%d",
		readyCount, pendingCount, totalCount, TargetCount, MinCount)

	// 只有当总数小于最小数时才触发注册，避免频繁注册
	if totalCount < MinCount {
		needCount := TargetCount - totalCount
		logger.Info("⚠️ 账号数低于最小值 (%d < %d)，需要注册 %d 个", totalCount, MinCount, needCount)
		if err := StartRegister(needCount); err != nil {
			logger.Error("❌ 启动注册失败: %v", err)
		}
	} else if totalCount < TargetCount {
		logger.Debug("📊 账号数未达目标 (%d < %d)，但高于最小值，暂不触发注册", totalCount, TargetCount)
	}
}
