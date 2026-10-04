// Package applog 提供统一日志：控制台（stdout）+ 按天分级文件日志。
//
// 日志文件规则：$ALIYUN_MONITOR_LOGS/YYYYMM/DD.log 与 $ALIYUN_MONITOR_LOGS/YYYYMM/DD.error.log。
// 所有日志同时输出到控制台；error 级别额外单独落盘一份。
// 默认日志目录：<ALIYUN_MONITOR_DATA 默认 data>/logs（即 data/logs）。
package applog

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 日志级别
type Level int

const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	}
	return "INFO"
}

// 全局状态
var (
	mu          sync.Mutex
	logsDir     string // 日志根目录（YYYYMM 子目录的父目录）
	consoleOut  io.Writer
	infoWriter  io.Writer // 当日 DD.log
	errorWriter io.Writer // 当日 DD.error.log
	openDate    string    // 已打开的日志对应日期 20060102
	openMonth   string    // 已打开的日志对应月份 200601
	initialized bool
)

// 兼容旧接口：包级默认 logger（所有消息按 INFO 处理 + error 级别落盘）
var std = log.New(os.Stdout, "", log.LstdFlags|log.Lmsgprefix)

// SetOutput 设置控制台输出（默认 os.Stdout；测试可替换）。
func SetOutput(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	consoleOut = w
}

// Setup 初始化日志系统。
//   - dataDir: 数据目录（ALIYUN_MONITOR_DATA），用于默认日志目录
//   - logsDirOverride: ALIYUN_MONITOR_LOGS；为空则用 dataDir/logs（与数据目录分开）
//
// 日志是可选功能：目录不可写时不阻塞主程序，只输出到控制台。
// 幂等：重复调用不会重复打开文件。
func Setup(dataDir, logsDirOverride string) {
	mu.Lock()
	defer mu.Unlock()

	if logsDirOverride != "" {
		logsDir = logsDirOverride
	} else {
		logsDir = filepath.Join(dataDir, "logs")
	}
	if consoleOut == nil {
		consoleOut = os.Stdout
	}
	openFilesForDate(time.Now())
	initialized = true
}

// openFilesForDate 按当天日期打开日志文件（YYYYMM/DD.log 与 DD.error.log）。
func openFilesForDate(now time.Time) {
	month := now.Format("200601")
	day := now.Format("20060102")
	if openDate == day && openMonth == month && infoWriter != nil {
		return
	}
	dir := filepath.Join(logsDir, month)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Printf("[applog] 创建日志目录失败 (%s): %v", dir, err)
		return
	}

	closeOld()

	infoF, err := os.OpenFile(filepath.Join(dir, day+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		log.Printf("[applog] 打开日志文件失败: %v", err)
	} else {
		infoWriter = infoF
	}
	errF, err := os.OpenFile(filepath.Join(dir, day+".error.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		log.Printf("[applog] 打开错误日志文件失败: %v", err)
	} else {
		errorWriter = errF
	}
	openDate = day
	openMonth = month
}

func closeOld() {
	if infoWriter != nil {
		if c, ok := infoWriter.(io.Closer); ok {
			c.Close()
		}
		infoWriter = nil
	}
	if errorWriter != nil {
		if c, ok := errorWriter.(io.Closer); ok {
			c.Close()
		}
		errorWriter = nil
	}
}

// rotateIfNeeded 跨天时轮转日志文件。
func rotateIfNeeded() {
	if !initialized {
		return
	}
	now := time.Now()
	if now.Format("20060102") != openDate {
		openFilesForDate(now)
	}
}

// Log 记录一条日志：控制台输出 + 按级别写入当日文件。
// error 级别消息同时写入 DD.log 与 DD.error.log。
func Log(level Level, msg string) {
	line := fmt.Sprintf("%s [%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), level, msg)

	mu.Lock()
	rotateIfNeeded()
	out := consoleOut
	if out == nil {
		out = os.Stdout
	}
	if infoWriter != nil {
		io.WriteString(infoWriter, line)
	}
	if level >= LevelError && errorWriter != nil {
		io.WriteString(errorWriter, line)
	}
	mu.Unlock()

	// 控制台：error 级别用 stderr，其余用 stdout
	var w io.Writer = out
	if level >= LevelError {
		w = os.Stderr
	}
	io.WriteString(w, line)
}

// Printf 记录 INFO 级别日志（兼容现有 log.Printf 语义）。
func Printf(format string, args ...interface{}) {
	Log(LevelInfo, fmt.Sprintf(format, args...))
}

// Warnf 记录 WARN 级别日志。
func Warnf(format string, args ...interface{}) {
	Log(LevelWarn, fmt.Sprintf(format, args...))
}

// Errorf 记录 ERROR 级别日志（同时写入 error 日志文件）。
func Errorf(format string, args ...interface{}) {
	Log(LevelError, fmt.Sprintf(format, args...))
}

// Fatalf 记录 ERROR 并退出。
func Fatalf(format string, args ...interface{}) {
	Log(LevelError, fmt.Sprintf(format, args...))
	os.Exit(1)
}

// IsErrorText 粗略判断消息是否属于错误级别（供旧 log.Printf 分流使用）。
// 注意：这是启发式，仅用于兼容层；新代码请直接使用 Errorf/Warnf。
func IsErrorText(msg string) bool {
	m := strings.ToLower(msg)
	markers := []string{"失败", "错误", "出错", "异常", "panic", "无法", "拒绝", "不存在", "未找到",
		"超时", "fail", "error", "fatal", "不可用", "不足", "无效", "错误:", "告警"}
	for _, mk := range markers {
		if strings.Contains(m, mk) {
			return true
		}
	}
	return false
}

// Hook 包级 log 输出，使现有 log.Printf 同时进入分级文件。
// 写法兼容旧代码：按消息内容启发式判定级别（错误类消息进入 error 日志）。
// 用法：log.SetOutput(applog.Hook{})
type Hook struct{}

func (Hook) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	level := LevelInfo
	if IsErrorText(msg) {
		level = LevelError
	}
	Log(level, msg)
	return len(p), nil
}

// Ensure 确保日志系统已初始化（未初始化时用默认值初始化）。
// 供各 internal 包在首次打日志前调用。
func Ensure(dataDir string) {
	mu.Lock()
	if !initialized {
		mu.Unlock()
		Setup(dataDir, DefaultLogsDir())
		return
	}
	mu.Unlock()
}

// Cleanup 关闭日志文件（进程退出时调用）。
func Cleanup() {
	mu.Lock()
	defer mu.Unlock()
	closeOld()
	initialized = false
}

// DefaultLogsDir 返回日志目录（环境变量 ALIYUN_MONITOR_LOGS，默认 logs）。
func DefaultLogsDir() string {
	if d := os.Getenv("ALIYUN_MONITOR_LOGS"); d != "" {
		return d
	}
	return "logs"
}

// ListLogFiles 返回 logsDir 下所有日志文件路径（按时间倒序），供管理命令使用。
func ListLogFiles() []string {
	var files []string
	if logsDir == "" {
		return files
	}
	filepath.Walk(logsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	sort.Strings(files)
	return files
}
