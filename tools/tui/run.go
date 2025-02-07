// License: GPLv3 Copyright: 2023, Kovid Goyal, <kovid at kovidgoyal.net>

package tui

import (
	"fmt"
	"kitty"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/shirou/gopsutil/v3/process"
	"golang.org/x/sys/unix"

	"kitty/tools/config"
	"kitty/tools/tty"
	"kitty/tools/tui/loop"
	"kitty/tools/tui/shell_integration"
	"kitty/tools/utils"
	"kitty/tools/utils/shlex"
)

var _ = fmt.Print

type KittyOpts struct {
	Shell, Shell_integration string
}

func read_relevant_kitty_opts(path string) KittyOpts {
	ans := KittyOpts{Shell: kitty.KittyConfigDefaults.Shell, Shell_integration: kitty.KittyConfigDefaults.Shell_integration}
	handle_line := func(key, val string) error {
		switch key {
		case "shell":
			ans.Shell = strings.TrimSpace(val)
		case "shell_integration":
			ans.Shell_integration = strings.TrimSpace(val)
		}
		return nil
	}
	cp := config.ConfigParser{LineHandler: handle_line}
	cp.ParseFiles(path)
	if ans.Shell == "" {
		ans.Shell = kitty.KittyConfigDefaults.Shell
	}
	return ans
}

func get_effective_ksi_env_var(x string) string {
	parts := strings.Split(strings.TrimSpace(strings.ToLower(x)), " ")
	current := utils.NewSetWithItems(parts...)
	if current.Has("disabled") {
		return ""
	}
	allowed := utils.NewSetWithItems(kitty.AllowedShellIntegrationValues...)
	if !current.IsSubsetOf(allowed) {
		return relevant_kitty_opts().Shell_integration
	}
	return x
}

var relevant_kitty_opts = sync.OnceValue(func() KittyOpts {
	return read_relevant_kitty_opts(filepath.Join(utils.ConfigDir(), "kitty.conf"))
})

func get_shell_from_kitty_conf() (shell string) {
	shell = relevant_kitty_opts().Shell
	if shell == "." {
		s, e := utils.LoginShellForCurrentUser()
		if e != nil {
			shell = "/bin/sh"
		} else {
			shell = s
		}
	}
	return
}

func find_shell_parent_process() string {
	var p *process.Process
	var err error
	for {
		if p == nil {
			p, err = process.NewProcess(int32(os.Getppid()))
		} else {
			p, err = p.Parent()
		}
		if err != nil {
			return ""
		}
		if cmdline, err := p.CmdlineSlice(); err == nil && len(cmdline) > 0 {
			exe := get_shell_name(filepath.Base(cmdline[0]))
			if shell_integration.IsSupportedShell(exe) {
				return exe
			}
		}
	}
}

func ResolveShell(shell string) []string {
	switch shell {
	case "":
		shell = get_shell_from_kitty_conf()
	case ".":
		if shell = find_shell_parent_process(); shell == "" {
			shell = get_shell_from_kitty_conf()
		}
	}
	shell_cmd, err := shlex.Split(shell)
	if err != nil {
		shell_cmd = []string{shell}
	}
	exe := utils.FindExe(shell_cmd[0])
	if unix.Access(exe, unix.X_OK) != nil {
		shell_cmd = []string{"/bin/sh"}
	}
	return shell_cmd
}

func ResolveShellIntegration(shell_integration string) string {
	if shell_integration == "" {
		shell_integration = relevant_kitty_opts().Shell_integration
	}
	return get_effective_ksi_env_var(shell_integration)
}

func get_shell_name(argv0 string) (ans string) {
	ans = filepath.Base(argv0)
	if strings.HasSuffix(strings.ToLower(ans), ".exe") {
		ans = ans[:len(ans)-4]
	}
	if strings.HasPrefix(ans, "-") {
		ans = ans[1:]
	}
	return
}

func rc_modification_allowed(ksi string) bool {
	for _, x := range strings.Split(ksi, " ") {
		switch x {
		case "disabled", "no-rc":
			return false
		}
	}
	return ksi != ""
}

func RunShell(shell_cmd []string, shell_integration_env_var_val string) (err error) {
	shell_name := get_shell_name(shell_cmd[0])
	var shell_env map[string]string
	if rc_modification_allowed(shell_integration_env_var_val) && shell_integration.IsSupportedShell(shell_name) {
		oenv := os.Environ()
		env := make(map[string]string, len(oenv))
		for _, x := range oenv {
			if k, v, found := strings.Cut(x, "="); found {
				env[k] = v
			}
		}
		argv, env, err := shell_integration.Setup(shell_name, shell_integration_env_var_val, shell_cmd, env)
		if err != nil {
			return err
		}
		shell_cmd = argv
		shell_env = env
	}
	exe := shell_cmd[0]
	if runtime.GOOS == "darwin" {
		// ensure shell runs in login mode. On macOS lots of people use ~/.bash_profile instead of ~/.bashrc
		// which means they expect the shell to run in login mode always. Le Sigh.
		shell_cmd[0] = "-" + filepath.Base(shell_cmd[0])
	}
	var env []string
	if shell_env != nil {
		env = make([]string, 0, len(shell_env))
		for k, v := range shell_env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
	} else {
		env = os.Environ()
	}
	// fmt.Println(fmt.Sprintf("%s %v\n%#v", utils.FindExe(exe), shell_cmd, env))
	return unix.Exec(utils.FindExe(exe), shell_cmd, env)
}

func RunCommandRestoringTerminalToSaneStateAfter(cmd []string) {
	exe := utils.FindExe(cmd[0])
	c := exec.Command(exe, cmd[1:]...)
	c.Stdout = os.Stdout
	c.Stdin = os.Stdin
	c.Stderr = os.Stderr
	term, err := tty.OpenControllingTerm()
	if err == nil {
		var state_before unix.Termios
		if term.Tcgetattr(&state_before) == nil {
			term.WriteString(loop.SAVE_PRIVATE_MODE_VALUES)
			defer func() {
				term.WriteString(strings.Join([]string{
					loop.RESTORE_PRIVATE_MODE_VALUES,
					"\x1b[=u",                      // reset kitty keyboard protocol to legacy
					"\x1b[1 q",                     // blinking block cursor
					loop.DECTCEM.EscapeCodeToSet(), // cursor visible
					"\x1b]112\a",                   // reset cursor color
				}, ""))
				term.Tcsetattr(tty.TCSANOW, &state_before)
				term.Close()
			}()
		} else {
			defer term.Close()
		}
	}
	err = c.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, cmd[0], "failed with error:", err)
	}
}
