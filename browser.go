package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// ==================== 浏览器自动化注册 ====================

var (
	// RegisterDebug 调试模式（截图）
	RegisterDebug bool
	// RegisterOnce 单次运行模式（调试用）
	RegisterOnce bool

	firstNames  = []string{"John", "Jane", "Michael", "Sarah", "David", "Emily", "Robert", "Lisa", "James", "Emma"}
	lastNames   = []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis", "Wilson", "Taylor"}
	commonWords = map[string]bool{
		"VERIFY": true, "GOOGLE": true, "UPDATE": true, "MOBILE": true, "DEVICE": true,
		"SUBMIT": true, "RESEND": true, "CANCEL": true, "DELETE": true, "REMOVE": true,
		"SEARCH": true, "VIDEOS": true, "IMAGES": true, "GMAIL": true, "EMAIL": true,
		"ACCOUNT": true, "CHROME": true,
	}
)

// TempEmailResponse 临时邮箱响应
type TempEmailResponse struct {
	Email string `json:"email"`
	Data  struct {
		Email string `json:"email"`
	} `json:"data"`
}

// EmailListResponse 邮件列表响应
type EmailListResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Emails []EmailContent `json:"emails"`
	} `json:"data"`
}

// EmailContent 邮件内容
type EmailContent struct {
	Subject string `json:"subject"`
	Content string `json:"content"`
}

// BrowserRegisterResult 注册结果
type BrowserRegisterResult struct {
	Success       bool
	Email         string
	FullName      string
	Authorization string
	Cookies       []Cookie
	ConfigID      string
	CSESIDX       string
	Error         error
}

// generateRandomName 生成随机全名
func generateRandomName() string {
	return firstNames[rand.Intn(len(firstNames))] + " " + lastNames[rand.Intn(len(lastNames))]
}

// getTemporaryEmail 获取临时邮箱
func getTemporaryEmail() (string, error) {
	req, _ := http.NewRequest("GET", "https://mail.chatgpt.org.uk/api/generate-email", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://mail.chatgpt.org.uk")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取临时邮箱失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := readResponseBody(resp)
	var result TempEmailResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析临时邮箱响应失败: %w", err)
	}

	email := result.Email
	if email == "" {
		email = result.Data.Email
	}
	if email == "" {
		return "", fmt.Errorf("获取临时邮箱为空")
	}
	return email, nil
}
func getVerificationEmailQuick(email string, retries int, intervalSec int) (*EmailContent, error) {
	for i := 0; i < retries; i++ {
		req, _ := http.NewRequest("GET", fmt.Sprintf("https://mail.chatgpt.org.uk/api/emails?email=%s", email), nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
		req.Header.Set("Referer", "https://mail.chatgpt.org.uk")

		resp, err := httpClient.Do(req)
		if err != nil {
			time.Sleep(time.Duration(intervalSec) * time.Second)
			continue
		}

		body, _ := readResponseBody(resp)
		resp.Body.Close()

		var result EmailListResponse
		if err := json.Unmarshal(body, &result); err != nil {
			time.Sleep(time.Duration(intervalSec) * time.Second)
			continue
		}

		if result.Success && len(result.Data.Emails) > 0 {
			return &result.Data.Emails[0], nil
		}
		time.Sleep(time.Duration(intervalSec) * time.Second)
	}
	return nil, fmt.Errorf("未收到验证码邮件")
}

func extractVerificationCode(content string) (string, error) {
	re := regexp.MustCompile(`\b[A-Z0-9]{6}\b`)
	matches := re.FindAllString(content, -1)

	for _, code := range matches {
		if commonWords[code] {
			continue
		}
		if regexp.MustCompile(`[0-9]`).MatchString(code) {
			return code, nil
		}
	}

	for _, code := range matches {
		if !commonWords[code] {
			return code, nil
		}
	}

	re2 := regexp.MustCompile(`(?i)code\s*[:is]\s*([A-Z0-9]{6})`)
	if m := re2.FindStringSubmatch(content); len(m) > 1 {
		return m[1], nil
	}

	return "", fmt.Errorf("无法从邮件中提取验证码")
}
func safeType(page *rod.Page, text string, delay int) error {
	for _, char := range text {
		if err := page.Keyboard.Type(input.Key(char)); err != nil {
			return err
		}
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}
	return nil
}

// debugScreenshot 调试截图
func debugScreenshot(page *rod.Page, threadID int, step string) {
	if !RegisterDebug {
		return
	}
	screenshotDir := filepath.Join(DataDir, "screenshots")
	os.MkdirAll(screenshotDir, 0755)

	filename := filepath.Join(screenshotDir, fmt.Sprintf("thread%d_%s_%d.png", threadID, step, time.Now().Unix()))
	data, err := page.Screenshot(true, nil)
	if err != nil {
		log.Printf("[注册 %d] 📸 截图失败: %v", threadID, err)
		return
	}
	if err := os.WriteFile(filename, data, 0644); err != nil {
		log.Printf("[注册 %d] 📸 保存截图失败: %v", threadID, err)
		return
	}
	log.Printf("[注册 %d] 📸 截图保存: %s", threadID, filename)
}

func RunBrowserRegister(headless bool, proxy string, threadID int) (result *BrowserRegisterResult) {
	result = &BrowserRegisterResult{}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[注册 %d] ☠️ panic 恢复: %v", threadID, r)
			result.Error = fmt.Errorf("panic: %v", r)
		}
	}()

	// 获取临时邮箱
	email, err := getTemporaryEmail()
	if err != nil {
		result.Error = err
		return result
	}
	result.Email = email

	// 启动浏览器 - 优先使用系统浏览器
	l := launcher.New()

	// 检测系统浏览器
	systemBrowsers := []string{
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/snap/bin/chromium",
		"/opt/google/chrome/chrome",
	}
	for _, path := range systemBrowsers {
		if _, err := os.Stat(path); err == nil {
			l = l.Bin(path)
			break
		}
	}

	l = l.Headless(headless).
		Set("no-sandbox").
		Set("disable-setuid-sandbox").
		Set("disable-dev-shm-usage").
		Set("disable-blink-features", "AutomationControlled").
		Set("window-size", "1280,800")

	if proxy != "" {
		l = l.Proxy(proxy)
	}

	url, err := l.Launch()
	if err != nil {
		result.Error = fmt.Errorf("启动浏览器失败: %w", err)
		return result
	}

	browser := rod.New().ControlURL(url)
	if err := browser.Connect(); err != nil {
		result.Error = fmt.Errorf("连接浏览器失败: %w", err)
		return result
	}
	defer browser.Close()

	browser = browser.Timeout(120 * time.Second)

	// 获取默认页面
	pages, _ := browser.Pages()
	var page *rod.Page
	if len(pages) > 0 {
		page = pages[0]
	} else {
		page, _ = browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	}

	// 设置视口和 User-Agent
	page.MustSetViewport(1280, 800, 1, false)
	page.SetUserAgent(&proto.NetworkSetUserAgentOverride{
		UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	})

	// 监听请求以捕获 authorization
	var authorization string
	var configID, csesidx string

	go page.EachEvent(func(e *proto.NetworkRequestWillBeSent) {
		if auth, ok := e.Request.Headers["authorization"]; ok {
			if authStr := auth.String(); authStr != "" {
				authorization = authStr
			}
		}
		url := e.Request.URL
		if m := regexp.MustCompile(`/cid/([a-f0-9-]+)`).FindStringSubmatch(url); len(m) > 1 && configID == "" {
			configID = m[1]
		}
		if m := regexp.MustCompile(`[?&]csesidx=(\d+)`).FindStringSubmatch(url); len(m) > 1 && csesidx == "" {
			csesidx = m[1]
		}
	})()
	if err := page.Navigate("https://business.gemini.google"); err != nil {
		result.Error = fmt.Errorf("打开页面失败: %w", err)
		return result
	}
	page.WaitLoad()
	time.Sleep(1 * time.Second)
	debugScreenshot(page, threadID, "01_page_loaded")
	if _, err := page.Timeout(30 * time.Second).Element("input"); err != nil {
		result.Error = fmt.Errorf("等待输入框超时: %w", err)
		return result
	}
	time.Sleep(1 * time.Second)

	// 点击输入框聚焦
	page.Eval(`() => {
		const inputs = document.querySelectorAll('input');
		if (inputs.length > 0) {
			inputs[0].click();
			inputs[0].focus();
		}
	}`)
	time.Sleep(500 * time.Millisecond)
	safeType(page, email, 20)
	time.Sleep(1 * time.Second)
	debugScreenshot(page, threadID, "02_email_input")

	// 触发 blur
	page.Eval(`() => {
		const inputs = document.querySelectorAll('input');
		if (inputs.length > 0) {
			inputs[0].blur();
		}
	}`)
	time.Sleep(500 * time.Millisecond)
	debugScreenshot(page, threadID, "03_before_submit")
	emailSubmitted := false
	for i := 0; i < 8; i++ {
		clickResult, _ := page.Eval(`() => {
			if (!document.body) return { clicked: false, reason: 'body_null' };
			
			const targets = ['继续', 'Next', '邮箱', 'Continue'];
			const elements = [
				...document.querySelectorAll('button'),
				...document.querySelectorAll('input[type="submit"]'),
				...document.querySelectorAll('div[role="button"]'),
				...document.querySelectorAll('span[role="button"]')
			];

			for (const element of elements) {
				if (!element) continue;
				const style = window.getComputedStyle(element);
				if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') continue;
				if (element.disabled) continue;

				const text = element.textContent ? element.textContent.trim() : '';
				if (targets.some(t => text.includes(t))) {
					element.click();
					return { clicked: true, text: text };
				}
			}
			return { clicked: false, reason: 'no_button' };
		}`)

		if clickResult != nil && clickResult.Value.Get("clicked").Bool() {
			emailSubmitted = true
			break
		}
		log.Printf("[注册 %d] 尝试 %d/8: 未找到按钮", threadID, i+1)
		time.Sleep(1 * time.Second)
	}
	if !emailSubmitted {
		result.Error = fmt.Errorf("找不到提交按钮")
		return result
	}
	time.Sleep(3 * time.Second)
	debugScreenshot(page, threadID, "04_after_submit")
	log.Printf("[注册 %d] 检查页面状态...", threadID)
	var needsVerification bool
	checkResult, _ := page.Eval(`() => {
		const pageText = document.body ? document.body.textContent : '';
		
		// 检查常见错误
		if (pageText.includes('出了点问题') || pageText.includes('Something went wrong') ||
			pageText.includes('无法创建') || pageText.includes('cannot create') ||
			pageText.includes('不安全') || pageText.includes('secure') ||
			pageText.includes('电话') || pageText.includes('Phone') || pageText.includes('number')) {
			return { error: true, text: document.body.innerText.substring(0, 100) };
		}

		// 检查是否需要验证码
		if (pageText.includes('验证') || pageText.includes('Verify') || 
			pageText.includes('code') || pageText.includes('sent')) {
			return { needsVerification: true, isNamePage: false };
		}
		
		// 检查是否已经到了姓名页面
		if (pageText.includes('姓氏') || pageText.includes('名字') || 
			pageText.includes('Full name') || pageText.includes('全名')) {
			return { needsVerification: false, isNamePage: true };
		}
		
		return { needsVerification: true, isNamePage: false };
	}`)

	if checkResult != nil {
		if checkResult.Value.Get("error").Bool() {
			errText := checkResult.Value.Get("text").String()
			result.Error = fmt.Errorf("页面显示错误: %s...", errText)
			log.Printf("[注册 %d] ❌ %v", threadID, result.Error)
			return result
		}
		needsVerification = checkResult.Value.Get("needsVerification").Bool()
		isNamePage := checkResult.Value.Get("isNamePage").Bool()
		log.Printf("[注册 %d] 页面状态: needsVerification=%v, isNamePage=%v", threadID, needsVerification, isNamePage)
	} else {
		needsVerification = true
	}

	// 处理验证码
	if needsVerification {

		var emailContent *EmailContent
		maxResendAttempts := 3

		for resendAttempt := 0; resendAttempt < maxResendAttempts; resendAttempt++ {
			emailContent, _ = getVerificationEmailQuick(email, 15, 2)

			if emailContent != nil {
				break
			}

			// 尝试点击重发
			if resendAttempt < maxResendAttempts-1 {
				debugScreenshot(page, threadID, fmt.Sprintf("05_no_code_attempt%d", resendAttempt+1))
				page.Eval(`() => {
					const resendTexts = ['重新发送', 'Resend', 'resend', '重发', 'Send again', '再次发送', '发送'];
					const elements = [
						...document.querySelectorAll('a'),
						...document.querySelectorAll('button'),
						...document.querySelectorAll('span'),
						...document.querySelectorAll('div[role="button"]')
					];
					
					for (const element of elements) {
						if (!element) continue;
						const text = element.textContent ? element.textContent.trim() : '';
						const style = window.getComputedStyle(element);
						if (style.display === 'none' || style.visibility === 'hidden') continue;
						
						if (text && resendTexts.some(t => text.toLowerCase().includes(t.toLowerCase()))) {
							element.click();
							return true;
						}
					}
					return false;
				}`)
				time.Sleep(3 * time.Second)
			}
		}

		if emailContent == nil {
			result.Error = fmt.Errorf("多次尝试后仍无法获取验证码邮件")
			return result
		}

		// 提取验证码
		code, err := extractVerificationCode(emailContent.Content)
		if err != nil {
			result.Error = err
			return result
		}

		// 等待验证码输入框
		time.Sleep(1 * time.Second)

		// 清空并聚焦输入框
		page.Eval(`() => {
			const inputs = document.querySelectorAll('input');
			if (inputs.length > 0) {
				inputs[0].value = '';
				inputs[0].click();
				inputs[0].focus();
			}
		}`)
		time.Sleep(500 * time.Millisecond)
		safeType(page, code, 20)
		time.Sleep(1 * time.Second)

		// 触发 blur
		page.Eval(`() => {
			const inputs = document.querySelectorAll('input');
			if (inputs.length > 0) {
				inputs[0].blur();
			}
		}`)
		time.Sleep(500 * time.Millisecond)

		for i := 0; i < 5; i++ {
			clickResult, _ := page.Eval(`() => {
				const targets = ['验证', 'Verify', '继续', 'Next', 'Continue'];
				const elements = [
					...document.querySelectorAll('button'),
					...document.querySelectorAll('input[type="submit"]'),
					...document.querySelectorAll('div[role="button"]')
				];

				for (const element of elements) {
					if (!element) continue;
					const style = window.getComputedStyle(element);
					if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') continue;
					if (element.disabled) continue;

					const text = element.textContent ? element.textContent.trim() : '';
					if (targets.some(t => text.includes(t))) {
						element.click();
						return { clicked: true, text: text };
					}
				}
				return { clicked: false };
			}`)

			if clickResult != nil && clickResult.Value.Get("clicked").Bool() {
				break
			}
			time.Sleep(1 * time.Second)
		}

		time.Sleep(3 * time.Second)
	}

	// 填写姓名
	fullName := generateRandomName()
	result.FullName = fullName
	log.Printf("[注册 %d] 填写姓名: %s", threadID, fullName)

	time.Sleep(1 * time.Second)

	// 清空并聚焦输入框
	page.Eval(`() => {
		const inputs = document.querySelectorAll('input');
		if (inputs.length > 0) {
			inputs[0].value = '';
			inputs[0].click();
			inputs[0].focus();
		}
	}`)
	time.Sleep(500 * time.Millisecond)

	// 输入姓名
	safeType(page, fullName, 20)
	time.Sleep(1 * time.Second)

	// 触发 blur
	page.Eval(`() => {
		const inputs = document.querySelectorAll('input');
		if (inputs.length > 0) {
			inputs[0].blur();
		}
	}`)
	time.Sleep(200 * time.Millisecond) // 优化等待时间
	for i := 0; i < 5; i++ {
		clickResult, _ := page.Eval(`() => {
			const targets = ['同意', 'Confirm', '继续', 'Next', 'Continue', 'I agree'];
			const elements = [
				...document.querySelectorAll('button'),
				...document.querySelectorAll('input[type="submit"]'),
				...document.querySelectorAll('div[role="button"]')
			];

			for (const element of elements) {
				if (!element) continue;
				const style = window.getComputedStyle(element);
				if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') continue;
				if (element.disabled) continue;

				const text = element.textContent ? element.textContent.trim() : '';
				if (targets.some(t => text.includes(t))) {
					element.click();
					return { clicked: true, text: text };
				}
			}

			// 备用：点击第一个可见按钮
			for (const element of elements) {
				if (element && element.offsetParent !== null && !element.disabled) {
					element.click();
					return { clicked: true, text: 'fallback' };
				}
			}
			return { clicked: false };
		}`)

		if clickResult != nil && clickResult.Value.Get("clicked").Bool() {
			break
		}
		time.Sleep(1500 * time.Millisecond)
	}

	// 等待获取 authorization
	log.Printf("[注册 %d] 等待获取 authorization...", threadID)
	for i := 0; i < 10; i++ {
		time.Sleep(3 * time.Second)

		// 尝试点击可能出现的额外按钮
		page.Eval(`() => {
			const buttons = document.querySelectorAll('button');
			for (const button of buttons) {
				if (!button) continue;
				const text = button.textContent || '';
				if (text.includes('同意') || text.includes('Confirm') || text.includes('继续') || 
					text.includes('Next') || text.includes('I agree')) {
					if (button.offsetParent !== null && !button.disabled) {
						button.click();
						return true;
					}
				}
			}
			return false;
		}`)

		// 从 URL 提取信息
		info, _ := page.Info()
		if info != nil {
			currentURL := info.URL
			if m := regexp.MustCompile(`/cid/([a-f0-9-]+)`).FindStringSubmatch(currentURL); len(m) > 1 && configID == "" {
				configID = m[1]
				log.Printf("[注册 %d] 从URL提取 configId: %s", threadID, configID)
			}
			if m := regexp.MustCompile(`[?&]csesidx=(\d+)`).FindStringSubmatch(currentURL); len(m) > 1 && csesidx == "" {
				csesidx = m[1]
				log.Printf("[注册 %d] 从URL提取 csesidx: %s", threadID, csesidx)
			}
		}

		if authorization != "" {
			break
		}
	}

	if authorization == "" {
		result.Error = fmt.Errorf("未能获取 Authorization")
		return result
	}

	// 获取 cookies
	cookies, _ := page.Cookies(nil)
	var resultCookies []Cookie
	for _, c := range cookies {
		resultCookies = append(resultCookies, Cookie{
			Name:   c.Name,
			Value:  c.Value,
			Domain: c.Domain,
		})
	}

	result.Success = true
	result.Authorization = authorization
	result.Cookies = resultCookies
	result.ConfigID = configID
	result.CSESIDX = csesidx

	log.Printf("[注册 %d] ✅ 注册成功: %s", threadID, email)
	return result
}

// SaveBrowserRegisterResult 保存注册结果
func SaveBrowserRegisterResult(result *BrowserRegisterResult, dataDir string) error {
	if !result.Success {
		return result.Error
	}

	data := AccountData{
		Email:         result.Email,
		FullName:      result.FullName,
		Authorization: result.Authorization,
		Cookies:       result.Cookies,
		ConfigID:      result.ConfigID,
		CSESIDX:       result.CSESIDX,
		Timestamp:     time.Now().Format(time.RFC3339),
	}

	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}

	filename := filepath.Join(dataDir, fmt.Sprintf("%s.json", result.Email))
	if err := os.WriteFile(filename, jsonData, 0644); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}

	return nil
}

// NativeRegisterWorker 原生 Go 注册 worker
func NativeRegisterWorker(id int, dataDirAbs string) {
	time.Sleep(time.Duration(id) * 3 * time.Second)

	for atomic.LoadInt32(&isRegistering) == 1 {
		if pool.TotalCount() >= appConfig.Pool.TargetCount {
			return
		}

		log.Printf("[注册线程 %d] 启动注册任务", id)

		result := RunBrowserRegister(appConfig.Pool.RegisterHeadless, Proxy, id)

		if result.Success {
			if err := SaveBrowserRegisterResult(result, dataDirAbs); err != nil {
				log.Printf("[注册线程 %d] ⚠️ 保存失败: %v", id, err)
				registerStats.AddFailed(err.Error())
			} else {
				registerStats.AddSuccess()
				pool.Load(DataDir)
			}
		} else {
			errMsg := "未知错误"
			if result.Error != nil {
				errMsg = result.Error.Error()
			}
			log.Printf("[注册线程 %d] ❌ 注册失败: %s", id, errMsg)
			registerStats.AddFailed(errMsg)

			if strings.Contains(errMsg, "频繁") || strings.Contains(errMsg, "rate") ||
				strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "连接") {
				waitTime := 10 + id*2
				log.Printf("[注册线程 %d] ⏳ 等待 %d 秒后重试...", id, waitTime)
				time.Sleep(time.Duration(waitTime) * time.Second)
			} else {
				time.Sleep(3 * time.Second)
			}
		}
	}
	log.Printf("[注册线程 %d] 停止", id)
}
