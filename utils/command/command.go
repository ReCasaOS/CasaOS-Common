package command

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	exec2 "github.com/ReCasaOS/CasaOS-Common/utils/exec"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"
)

// Deprecated: This method is not safe, sould have ensure input.
func OnlyExec(cmdStr string) (string, error) {
	cmd := exec.Command("/bin/bash", "-c", cmdStr)
	buf, err := cmd.CombinedOutput()
	return string(buf), err
}

func ExecResultStr(cmdStr string) (string, error) {
	cmds := strings.Fields(cmdStr)
	cmd := exec2.Command(cmds[0], cmds[1:]...)

	output, err := cmd.CombinedOutput()
	return string(output), err
}

func ExecResultStrArray(cmdStr string) ([]string, error) {
	cmds := strings.Fields(cmdStr)
	cmd := exec.Command(cmds[0], cmds[1:]...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}

	result := strings.Split(string(output), "\n")
	return result, nil
}

// ScriptTimeout bounds each script ExecuteScripts runs: a hung script would
// otherwise hold the start of the service until systemd kills it, and then
// again at every restart. A variable so tests can shorten it.
var ScriptTimeout = 60 * time.Second

const (
	// scriptOutputLimit is how much of a script's output goes to the log: the
	// end of it, where an error usually is.
	scriptOutputLimit = 4 << 10

	// scriptWaitDelay is how long a script that has exited, or been killed, may
	// keep its output open through something it started, such as a daemon.
	scriptWaitDelay = 2 * time.Second
)

// ExecuteScripts runs every regular file of scriptDirectory, in name order,
// each with the interpreter of its shebang line (/bin/sh without one) and for
// at most ScriptTimeout. Every run is logged with its output. A failing script
// does not stop the next ones; the failures come back joined. A missing
// directory is not an error.
func ExecuteScripts(scriptDirectory string) error {
	entries, err := os.ReadDir(scriptDirectory)
	if errors.Is(err, fs.ErrNotExist) {
		logger.Info("no start scripts to run", zap.String("directory", scriptDirectory))
		return nil
	}
	if err != nil {
		logger.Error("cannot read the start scripts", zap.String("directory", scriptDirectory), zap.Error(err))
		return err
	}

	var failures []error
	for _, entry := range entries {
		path := filepath.Join(scriptDirectory, entry.Name())
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}

		result := runScript(path)
		reportScript(result)
		if result.err != nil {
			failures = append(failures, fmt.Errorf("start script %s: %w", result.name, result.err))
		}
	}

	return errors.Join(failures...)
}

type scriptResult struct {
	name        string
	interpreter string
	exitCode    int
	duration    time.Duration
	output      string
	err         error
}

// reportScript logs what a script did; tests replace it.
var reportScript = func(r scriptResult) {
	fields := []zap.Field{
		zap.String("script", r.name),
		zap.String("interpreter", r.interpreter),
		zap.Int("exit_code", r.exitCode),
		zap.Duration("duration", r.duration),
		zap.String("output", r.output),
	}
	if r.err != nil {
		logger.Error("start script failed", append(fields, zap.Error(r.err))...)
		return
	}
	logger.Info("start script ran", fields...)
}

func runScript(path string) scriptResult {
	result := scriptResult{name: filepath.Base(path), exitCode: -1}

	line, err := firstLine(path)
	if err != nil {
		result.err = err
		return result
	}
	argv := interpreter(line)
	result.interpreter = strings.Join(argv, " ")

	ctx, cancel := context.WithTimeout(context.Background(), ScriptTimeout)
	defer cancel()

	var output tail
	cmd := exec2.CommandContext(ctx, argv[0], append(argv[1:], path)...)
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.WaitDelay = scriptWaitDelay
	killGroupOnCancel(cmd.Cmd)

	start := time.Now()
	err = cmd.Run()
	result.duration = time.Since(start)
	result.exitCode = cmd.ProcessState.ExitCode()
	result.output = output.String()

	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		// ErrWaitDelay: the script exited well, but something it started still
		// held its output. Starting a daemon is not a failure.
	case ctx.Err() != nil:
		result.err = fmt.Errorf("timed out after %s: %w", ScriptTimeout, err)
	default:
		result.err = err
	}

	return result
}

func firstLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Scan()

	return scanner.Text(), scanner.Err()
}

// interpreter reads a shebang line as the kernel does: a program and at most
// one argument, so "#!/usr/bin/env bash" is /usr/bin/env with the argument
// bash. A script without one runs with /bin/sh.
func interpreter(line string) []string {
	rest, ok := strings.CutPrefix(line, "#!")
	rest = strings.TrimSpace(rest)
	if !ok || rest == "" {
		return []string{"/bin/sh"}
	}

	i := strings.IndexAny(rest, " \t")
	if i < 0 {
		return []string{rest}
	}

	return []string{rest[:i], strings.TrimSpace(rest[i+1:])}
}

// tail keeps the last scriptOutputLimit bytes written to it.
type tail struct {
	buf       []byte
	truncated bool
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - scriptOutputLimit; over > 0 {
		t.buf, t.truncated = t.buf[over:], true
	}

	return len(p), nil
}

func (t *tail) String() string {
	output := strings.TrimSpace(string(t.buf))
	if t.truncated {
		return "..." + output
	}

	return output
}

func ExecStdin(stdinStr string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	cmd.Stdin = bytes.NewBufferString(stdinStr)

	err := cmd.Run()
	if err != nil {
		fmt.Printf("Failed to execute command %s: %s\n", cmd.String(), err.Error())
		return "", err
	}

	return buf.String(), nil
}
