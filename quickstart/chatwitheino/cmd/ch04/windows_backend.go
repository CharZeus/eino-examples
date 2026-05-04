package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/schema"
)

const defaultRootPath = "/"

type Config struct {
	ValidateCommand func(string) error
}

var defaultValidateCommand = func(string) error {
	return nil
}

type WindowsBackend struct {
	validateCommand func(string) error
	RootDir         string
}

func NewBackend(_ context.Context, cfg *Config) (*WindowsBackend, error) {
	if cfg == nil {
		return nil, errors.New("config is required")
	}

	validateCommand := defaultValidateCommand
	if cfg.ValidateCommand != nil {
		validateCommand = cfg.ValidateCommand
	}

	return &WindowsBackend{
		validateCommand: validateCommand,
	}, nil
}

// resolvePath 处理路径，确保是绝对路径并处理 RootDir
func (win *WindowsBackend) resolvePath(p string) string {
	// 简单的路径清洗
	cleanPath := filepath.Clean(p)

	// 如果不是绝对路径，且配置了 RootDir，则拼接
	if !filepath.IsAbs(cleanPath) && win.RootDir != "" {
		return filepath.Join(win.RootDir, cleanPath)
	}
	return cleanPath
}

func (win *WindowsBackend) LsInfo(ctx context.Context, req *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	path := win.resolvePath(req.Path)

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", path, err)
	}

	var infos []filesystem.FileInfo
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		// 构建绝对路径用于返回，或者根据需求返回相对路径
		fullPath := filepath.Join(path, info.Name())

		infos = append(infos, filesystem.FileInfo{
			Path:  fullPath, // 注意：Windows下这里会是反斜杠，建议统一转为正斜杠
			IsDir: entry.IsDir(),
			Size:  info.Size(),
		})
	}
	return infos, nil
}

// Read reads file content with support for line-based offset and limit.
//
// Returns:
//   - string: The file content read
//   - error: Error if file does not exist or read fails
func (win *WindowsBackend) Read(ctx context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	path := win.resolvePath(req.FilePath)

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)

	// 简单的分页逻辑：跳过 Offset-1 行，读取 Limit 行
	// 注意：v0.8+ 中 Offset 是 1-based
	currentLine := 1
	count := 0

	for scanner.Scan() {
		if currentLine < req.Offset {
			currentLine++
			continue
		}
		if req.Limit > 0 && count >= req.Limit {
			break
		}
		lines = append(lines, scanner.Text())
		currentLine++
		count++
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// 返回 FileContent 结构体 (v0.8+ 变更)
	return &filesystem.FileContent{
		Content: strings.Join(lines, "\n"),
		// 如果需要，可以填充 StartLine 等字段
	}, nil
}

// GrepRaw searches for content matching the specified pattern in files.
//
// Returns:
//   - []GrepMatch: List of all matching results
//   - error: Error if the search fails
func (win *WindowsBackend) GrepRaw(ctx context.Context, req *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	searchPath := win.resolvePath(req.Path)
	pattern := req.Pattern
	globPattern := req.Glob

	// 检查路径是否存在
	info, err := os.Stat(searchPath)
	if err != nil {
		return nil, fmt.Errorf("path not found: %w", err)
	}

	var matches []filesystem.GrepMatch

	// 定义递归搜索函数
	var searchDir func(dir string) error
	searchDir = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			// 跳过隐藏目录和常见忽略目录
			if entry.IsDir() && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules") {
				continue
			}

			fullPath := filepath.Join(dir, entry.Name())

			if entry.IsDir() {
				if err := searchDir(fullPath); err != nil {
					return err
				}
			} else {
				// 文件匹配逻辑
				// 1. 检查 Glob 模式 (如 *.go)
				if globPattern != "" {
					matched, err := filepath.Match(globPattern, entry.Name())
					if err != nil || !matched {
						continue
					}
				}

				// 2. 读取文件内容并搜索
				content, err := os.ReadFile(fullPath)
				if err != nil {
					continue // 跳过无法读取的文件
				}

				lines := strings.Split(string(content), "\n")
				for i, line := range lines {
					// 简单的字符串包含匹配 (如需正则请使用 regexp.MatchString)
					if strings.Contains(line, pattern) {
						matches = append(matches, filesystem.GrepMatch{
							Path:    fullPath,
							Content: line,
							Line:    i + 1, // 行号从 1 开始
						})
					}
				}
			}
		}
		return nil
	}

	if info.IsDir() {
		if err := searchDir(searchPath); err != nil {
			return nil, err
		}
	} else {
		// 如果是单文件搜索
		content, err := os.ReadFile(searchPath)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			if strings.Contains(line, pattern) {
				matches = append(matches, filesystem.GrepMatch{
					Path:    searchPath,
					Content: line,
					Line:    i + 1,
				})
			}
		}
	}

	return matches, nil
}

// GlobInfo returns file information matching the glob pattern.
//
// Returns:
//   - []FileInfo: List of matching file information
//   - error: Error if the pattern is invalid or operation fails
func (win *WindowsBackend) GlobInfo(ctx context.Context, req *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	pattern := win.resolvePath(req.Pattern)

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	var infos []filesystem.FileInfo
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			continue
		}
		infos = append(infos, filesystem.FileInfo{
			Path:  match,
			IsDir: info.IsDir(),
			Size:  info.Size(),
		})
	}
	return infos, nil
}

// Write creates or updates file content.
//
// Returns:
//   - error: Error if the write operation fails
func (win *WindowsBackend) Write(ctx context.Context, req *filesystem.WriteRequest) error {
	path := win.resolvePath(req.FilePath)

	// 确保目录存在
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// v0.8+ 行为变更：文件存在则覆盖
	return os.WriteFile(path, []byte(req.Content), 0644)
}

// Edit replaces string occurrences in a file.
//
// Returns:
//   - error: Error if file does not exist, OldString is empty, or OldString is not found
func (win *WindowsBackend) Edit(ctx context.Context, req *filesystem.EditRequest) error {
	path := win.resolvePath(req.FilePath)

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	oldStr := req.OldString
	newStr := req.NewString

	var newContent []byte
	if req.ReplaceAll {
		newContent = bytes.ReplaceAll(content, []byte(oldStr), []byte(newStr))
	} else {
		newContent = bytes.Replace(content, []byte(oldStr), []byte(newStr), 1)
	}

	// 如果没有发生变化，根据业务逻辑决定是否报错，这里直接写入
	return os.WriteFile(path, newContent, 0644)
}

func (win *WindowsBackend) ExecuteStreaming(ctx context.Context, input *filesystem.ExecuteRequest) (result *schema.StreamReader[*filesystem.ExecuteResponse], err error) {
	if input.Command == "" {
		return nil, fmt.Errorf("command is required")
	}

	if err := win.validateCommand(input.Command); err != nil {
		return nil, err
	}

	cmd, stdout, stderr, err := win.initStreamingCmd(ctx, input.Command)
	if err != nil {
		return nil, err
	}

	sr, w := schema.Pipe[*filesystem.ExecuteResponse](100)

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		go sendErrorAndClose(w, fmt.Errorf("failed to start command: %w", err))
		return sr, nil
	}

	if input.RunInBackendGround {
		win.runCmdInBackground(ctx, cmd, stdout, stderr, w)
		return sr, nil
	}

	go win.streamCmdOutput(ctx, cmd, stdout, stderr, w)

	return sr, nil
}

// initStreamingCmd creates command with stdout and stderr pipes.
func (win *WindowsBackend) initStreamingCmd(ctx context.Context, command string) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, nil, nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	return cmd, stdout, stderr, nil
}

// runCmdInBackground executes command in background without waiting for completion.
// The caller controls timeout/cancellation via ctx.Done().
func (win *WindowsBackend) runCmdInBackground(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.ReadCloser, w *schema.StreamWriter[*filesystem.ExecuteResponse]) {
	go func() {
		defer func() {
			if pe := recover(); pe != nil {
				_ = cmd.Process.Kill()
			}
			_ = stdout.Close()
			_ = stderr.Close()
		}()

		done := make(chan struct{})
		go func() {
			drainPipesConcurrently(stdout, stderr)
			_ = cmd.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		}
	}()

	go func() {
		defer w.Close()
		w.Send(&filesystem.ExecuteResponse{Output: "command started in background\n", ExitCode: new(int)}, nil)
	}()
}

// drainPipesConcurrently consumes stdout and stderr concurrently to prevent pipe blocking.
func drainPipesConcurrently(stdout, stderr io.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.Discard, stdout)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.Discard, stderr)
	}()
	wg.Wait()
}

// streamCmdOutput handles streaming command output to the writer.
func (win *WindowsBackend) streamCmdOutput(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.ReadCloser, w *schema.StreamWriter[*filesystem.ExecuteResponse]) {
	defer func() {
		if pe := recover(); pe != nil {
			w.Send(nil, newPanicErr(pe, debug.Stack()))
			return
		}
		w.Close()
	}()

	stderrData, stderrErr := win.readStderrAsync(stderr)

	hasOutput, err := win.streamStdout(ctx, cmd, stdout, w)
	if err != nil {
		w.Send(nil, err)
		return
	}

	if stdError := <-stderrErr; stdError != nil {
		w.Send(nil, stdError)
		return
	}

	win.handleCmdCompletion(cmd, stderrData, hasOutput, w)
}

// readStderrAsync reads stderr in a separate goroutine.
func (win *WindowsBackend) readStderrAsync(stderr io.Reader) (*[]byte, <-chan error) {
	stderrData := new([]byte)
	stderrErr := make(chan error, 1)

	go func() {
		defer func() {
			if pe := recover(); pe != nil {
				stderrErr <- newPanicErr(pe, debug.Stack())
				return
			}
			close(stderrErr)
		}()
		var err error
		*stderrData, err = io.ReadAll(stderr)
		if err != nil {
			stderrErr <- fmt.Errorf("failed to read stderr: %w", err)
		}
	}()

	return stderrData, stderrErr
}

// streamStdout streams stdout line by line to the writer.
func (win *WindowsBackend) streamStdout(ctx context.Context, cmd *exec.Cmd, stdout io.Reader, w *schema.StreamWriter[*filesystem.ExecuteResponse]) (bool, error) {
	scanner := bufio.NewScanner(stdout)
	hasOutput := false

	for scanner.Scan() {
		hasOutput = true
		line := scanner.Text() + "\n"
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return hasOutput, ctx.Err()
		default:
			w.Send(&filesystem.ExecuteResponse{Output: line}, nil)
		}
	}

	if err := scanner.Err(); err != nil {
		return hasOutput, fmt.Errorf("error reading stdout: %w", err)
	}

	return hasOutput, nil
}

// handleCmdCompletion handles command completion and sends final response.
func (win *WindowsBackend) handleCmdCompletion(cmd *exec.Cmd, stderrData *[]byte, hasOutput bool, w *schema.StreamWriter[*filesystem.ExecuteResponse]) {
	if err := cmd.Wait(); err != nil {
		exitCode := 0
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
		if len(*stderrData) > 0 {
			w.Send(nil, fmt.Errorf("command exited with non-zero code %d: %s", exitCode, string(*stderrData)))
			return
		}
		w.Send(nil, fmt.Errorf("command exited with non-zero code %d", exitCode))
		return
	}

	if !hasOutput {
		w.Send(&filesystem.ExecuteResponse{ExitCode: new(int)}, nil)
	}
}

// sendErrorAndClose sends an error to the stream and closes it.
func sendErrorAndClose(w *schema.StreamWriter[*filesystem.ExecuteResponse], err error) {
	defer w.Close()
	w.Send(nil, err)
}

type panicErr struct {
	info  any
	stack []byte
}

func (p *panicErr) Error() string {
	return fmt.Sprintf("panic error: %v, \nstack: %s", p.info, string(p.stack))
}

func newPanicErr(info any, stack []byte) error {
	return &panicErr{
		info:  info,
		stack: stack,
	}
}
