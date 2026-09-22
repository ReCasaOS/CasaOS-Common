package command

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
)

func TestCommand(t *testing.T) {
	tests := []struct {
		name   string
		cmdStr string
		noErr  bool
	}{
		{
			name:   "Test Command",
			cmdStr: "ls -l",
			noErr:  true,
		},
		{
			name:   "Test Command with noescape",
			cmdStr: "ls -l whoami",
		},
		{
			name:   "Test Command with error",
			cmdStr: "`whoami` -l /test",
		},
		{
			name:   "Test Command with injection",
			cmdStr: "ls -l `whoami`",
		},
		{
			name:   "Test Command with multiple injection",
			cmdStr: "ls -l ; whoami",
		},
		{
			name:   "Test Command with multiple injection 2",
			cmdStr: "ls -l ; whoami",
		},
		{
			name:   "Test Command with injection on name",
			cmdStr: "ls ;whoami -l",
		},
		{
			name:   "Test Command with injection on arg-1",
			cmdStr: "ls -l;whoami",
		},
		{
			name:   "Test Command with quotation injection",
			cmdStr: "ls -l \" \"whoami",
		},
		{
			name:   "Test Command with injection shell script",
			cmdStr: "source /etc/local-storage-helper.sh ;USB_Stop_Auto",
		},
		{
			name:   "Test Command with injection shell script divided args",
			cmdStr: "source /etc/local-storage-helper.sh ; USB_Stop_Auto",
		},
		{
			name:   "Test Command with new-line injection shell script",
			cmdStr: "source /etc/local-storage-helper.sh\nenv",
		},
		{
			name:   "Delete SMB User",
			cmdStr: "smbpasswd -x testuser",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if output, err := ExecResultStr(tt.cmdStr); tt.noErr != (err == nil) {
				t.Errorf("ExecResultStr() error = %v, wantErr %v", err, tt.noErr)
			} else {
				t.Logf("Output: %s", output)
			}
		})

		t.Run(tt.name, func(t *testing.T) {
			if output, error := ExecResultStrArray(tt.cmdStr); tt.noErr != (error == nil) {
				t.Errorf("ExecResultStrArray() error = %v, wantErr %v", error, tt.noErr)
			} else {
				t.Logf("Output: %v", output)
			}
		})
	}
}

func TestTheShebangIsReadLikeTheKernelDoes(t *testing.T) {
	for line, want := range map[string][]string{
		"#!/bin/sh":                  {"/bin/sh"},
		"#!/bin/bash\r":              {"/bin/bash"},
		"#! /bin/bash -e":            {"/bin/bash", "-e"},
		"#!/usr/bin/env bash":        {"/usr/bin/env", "bash"},
		"#!/usr/bin/env\tpython3 -u": {"/usr/bin/env", "python3 -u"},
		"#!":                         {"/bin/sh"},
		"echo no shebang":            {"/bin/sh"},
		"":                           {"/bin/sh"},
	} {
		if got := interpreter(line); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q, want %q", line, got, want)
		}
	}
}

func TestOnlyTheEndOfALongOutputIsKept(t *testing.T) {
	var output tail
	output.Write([]byte(strings.Repeat("a", scriptOutputLimit)))
	output.Write([]byte(strings.Repeat("b", 100) + "the error\n"))

	got := output.String()
	if len(got) > scriptOutputLimit+3 || !strings.HasPrefix(got, "...a") || !strings.HasSuffix(got, "bthe error") {
		t.Fatalf("got %d bytes ending in %q", len(got), got[len(got)-20:])
	}
}

func TestAMissingScriptDirectoryIsNotAnError(t *testing.T) {
	logger.LogInitConsoleOnly()

	if err := ExecuteScripts(filepath.Join(t.TempDir(), "start.d")); err != nil {
		t.Fatal(err)
	}

	// The default report logs through zap, success and failure alike.
	reportScript(scriptResult{name: "10-ok.sh", interpreter: "/bin/sh", output: "done"})
	reportScript(scriptResult{name: "20-ko.sh", interpreter: "/bin/sh", exitCode: 1, err: os.ErrInvalid})
}

// runScripts writes the scripts to a directory, runs it and returns what was
// reported for each script, in order.
func runScripts(t *testing.T, scripts map[string]string) ([]scriptResult, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the scripts need /bin/sh and /usr/bin/env; CI runs this on Linux")
	}

	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "00-a-directory"), 0o755); err != nil {
		t.Fatal(err)
	}

	var results []scriptResult
	previous := reportScript
	reportScript = func(r scriptResult) { results = append(results, r) }
	t.Cleanup(func() { reportScript = previous })

	err := ExecuteScripts(dir)

	return results, err
}

func TestEachScriptRunsWithItsOwnInterpreter(t *testing.T) {
	results, err := runScripts(t, map[string]string{
		"10-sh.sh":   "#!/bin/sh\necho plain sh\n",
		"20-env.sh":  "#!/usr/bin/env sh\necho through env >&2\n",
		"30-none.sh": "echo no shebang\n",
		// cat prints the script itself: proof the shebang's program is what ran
		"40-cat.txt": "#!/bin/cat\nhello from cat\n",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := [][3]string{
		{"10-sh.sh", "/bin/sh", "plain sh"},
		{"20-env.sh", "/usr/bin/env sh", "through env"},
		{"30-none.sh", "/bin/sh", "no shebang"},
		{"40-cat.txt", "/bin/cat", "#!/bin/cat\nhello from cat"},
	}
	if len(results) != len(want) {
		t.Fatalf("got %d scripts, want %d: %+v", len(results), len(want), results)
	}
	for i, r := range results {
		if got := [3]string{r.name, r.interpreter, r.output}; got != want[i] || r.exitCode != 0 || r.err != nil {
			t.Errorf("got %q (exit %d, %v), want %q", got, r.exitCode, r.err, want[i])
		}
	}
}

func TestAFailingScriptDoesNotStopTheNextOnes(t *testing.T) {
	results, err := runScripts(t, map[string]string{
		"10-fails.sh": "#!/bin/sh\necho broken >&2\nexit 3\n",
		"20-next.sh":  "#!/bin/sh\necho still ran\n",
	})
	if err == nil || !strings.Contains(err.Error(), "10-fails.sh") {
		t.Fatalf("the failure must come back, got %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("got %d scripts, want 2: %+v", len(results), results)
	}
	if r := results[0]; r.err == nil || r.exitCode != 3 || r.output != "broken" {
		t.Errorf("failing script: got exit %d, output %q, %v", r.exitCode, r.output, r.err)
	}
	if r := results[1]; r.err != nil || r.output != "still ran" {
		t.Errorf("next script: got output %q, %v", r.output, r.err)
	}
}

func TestAHungScriptIsKilledAtItsTimeout(t *testing.T) {
	previous := ScriptTimeout
	ScriptTimeout = 200 * time.Millisecond
	t.Cleanup(func() { ScriptTimeout = previous })

	results, err := runScripts(t, map[string]string{
		"10-hangs.sh": "#!/bin/sh\necho started\nsleep 30\n",
		"20-next.sh":  "#!/bin/sh\necho still ran\n",
	})
	if err == nil || !strings.Contains(err.Error(), "10-hangs.sh: timed out") {
		t.Fatalf("the timeout must come back, got %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("got %d scripts, want 2: %+v", len(results), results)
	}
	hung := results[0]
	if hung.err == nil || hung.output != "started" {
		t.Errorf("hung script: got output %q, %v", hung.output, hung.err)
	}
	// Past scriptWaitDelay, the sleep outlived the shell: the group was not killed.
	if hung.duration >= scriptWaitDelay {
		t.Errorf("the hung script held the start for %s", hung.duration)
	}
	if r := results[1]; r.err != nil || r.output != "still ran" {
		t.Errorf("next script: got output %q, %v", r.output, r.err)
	}
}
