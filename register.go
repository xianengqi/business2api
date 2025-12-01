package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ==================== 注册与刷新 ====================

var isRegistering int32

// 注册统计
type RegisterStats struct {
	Total     int       `json:"total"`
	Success   int       `json:"success"`
	Failed    int       `json:"failed"`
	LastError string    `json:"lastError"`
	UpdatedAt time.Time `json:"updatedAt"`
	mu        sync.RWMutex
}

var registerStats = &RegisterStats{}

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

func startRegister(count int) error {
	if !atomic.CompareAndSwapInt32(&isRegistering, 0, 1) {
		return fmt.Errorf("注册进程已在运行")
	}

	// 获取数据目录的绝对路径
	dataDirAbs, _ := filepath.Abs(DataDir)
	if err := os.MkdirAll(dataDirAbs, 0755); err != nil {
		atomic.StoreInt32(&isRegistering, 0)
		return fmt.Errorf("创建数据目录失败: %w", err)
	}

	// 使用配置的线程数
	threads := appConfig.Pool.RegisterThreads
	if threads <= 0 {
		threads = 1
	}

	log.Printf("📝 启动 %d 个注册线程 (原生Go)，目标: %d 个，当前: %d 个", threads, appConfig.Pool.TargetCount, pool.TotalCount())

	for i := 0; i < threads; i++ {
		go NativeRegisterWorker(i+1, dataDirAbs)
	}

	// 监控进度
	go func() {
		for {
			time.Sleep(10 * time.Second)
			pool.Load(DataDir)
			if pool.TotalCount() >= appConfig.Pool.TargetCount {
				log.Printf("✅ 已达到目标账号数: %d，停止注册", pool.TotalCount())
				atomic.StoreInt32(&isRegistering, 0)
				return
			}
		}
	}()

	return nil
}

func poolMaintainer() {
	interval := time.Duration(appConfig.Pool.CheckIntervalMinutes) * time.Minute
	if interval < time.Minute {
		interval = 30 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	checkAndMaintainPool()

	for range ticker.C {
		checkAndMaintainPool()
	}
}

func checkAndMaintainPool() {
	pool.Load(DataDir)

	readyCount := pool.ReadyCount()
	pendingCount := pool.PendingCount()
	totalCount := pool.TotalCount()

	log.Printf("📊 号池检查: ready=%d, pending=%d, total=%d, 目标=%d, 最小=%d",
		readyCount, pendingCount, totalCount, appConfig.Pool.TargetCount, appConfig.Pool.MinCount)

	if totalCount < appConfig.Pool.TargetCount {
		needCount := appConfig.Pool.TargetCount - totalCount
		log.Printf("⚠️ 账号数未达目标，需要注册 %d 个", needCount)
		if err := startRegister(needCount); err != nil {
			log.Printf("❌ 启动注册失败: %v", err)
		}
	}
}
