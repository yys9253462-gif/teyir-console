package executor

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestPresetsAreFixedAndLabelled(t *testing.T) {
	for _, name := range []string{"update", "clean", "swap_1g", "timezone_shanghai", "install_fail2ban", "security_audit", "net_route", "uninstall_hy2", "kernel_reboot"} {
		p, ok := systemPresets[name]
		if !ok {
			t.Fatalf("preset %s missing", name)
		}
		if p.Label == "" || strings.TrimSpace(p.Script) == "" {
			t.Fatalf("preset %s has empty label or script", name)
		}
	}
	if _, ok := PresetNames()["rm -rf /"]; ok {
		t.Fatal("arbitrary names must not be accepted")
	}
}

func TestPresetScriptsParseInBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	for name, p := range systemPresets {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader("set -o pipefail\n" + p.Script + "\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("preset %s has a bash syntax error: %v\n%s", name, err, out)
		}
	}
}

// 2026-11-04：预设任务只回传最后 400 行，且整条管道在 bash 里（dash 不认 pipefail）。
func TestPresetCommandKeepsTailAndExitCode(t *testing.T) {
	src, err := os.ReadFile("presets.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `command := fmt.Sprintf("bash -c 'set -o pipefail; echo %s | base64 -d | %sbash 2>&1 | tail -n 400'", encoded, sudo)`) {
		t.Fatal("preset command must keep the output tail under bash pipefail")
	}
}

// 2026-11-04：会改系统的预设，关键命令失败时必须以非零退出（runPreset 只看退出码）。
// 原来脚本最后一行是 echo：apt 被锁住、swapon 失败、timedatectl 失败时任务照样记「成功」。
// 用假 apt-get / swapon / timedatectl（一律失败）放在 PATH 最前面跑真实脚本。
func TestMutatingPresetsFailWhenTheirCommandFails(t *testing.T) {
	// 视窗宿主下的命令行路径分隔符与类兼容层不同，避免误调子系统真实命令。
	if runtime.GOOS == "windows" {
		t.Skip("类系统脚本桩路径测试仅在非视窗环境运行")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	stubs := t.TempDir()
	for _, name := range []string{"apt-get", "mkswap", "swapon", "timedatectl", "fallocate"} {
		if err := os.WriteFile(stubs+"/"+name, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// swap_1g 先看「已有 swap」与磁盘空间：让 swapon --noheadings 输出为空、df 报足够空间。
	if err := os.WriteFile(stubs+"/df", []byte("#!/bin/sh\necho 'Filesystem 1024-blocks Used Available Capacity Mounted'\necho '/dev/x 100000000 1 99999999 1% /'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stubs+"/dd", []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"update", "kernel_reboot", "swap_1g", "timezone_shanghai"} {
		script := systemPresets[name].Script
		// 不让测试碰真实的 /swapfile、/etc/fstab。
		script = strings.ReplaceAll(script, "/swapfile", stubs+"/swapfile")
		script = strings.ReplaceAll(script, "/etc/fstab", stubs+"/fstab")
		cmd := exec.Command(bash, "-c", "set -o pipefail\n"+script)
		cmd.Env = append(os.Environ(), "PATH="+stubs+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("preset %s must exit non-zero when its command fails; output:\n%s", name, out)
		}
	}
	if b, _ := os.ReadFile(stubs + "/fstab"); strings.Contains(string(b), "swapfile") {
		t.Error("a failed swapon must not leave a swap entry in fstab")
	}
}

// 验证内核重启预设同时保护 teyir-console 与旧版 hy2-ops-console 面板宿主机，并校验非法 JSON 参数直接拦截。
func TestKernelRebootProtectsTeyirConsoleAndInvalidPresetJSONRejected(t *testing.T) {
	script := systemPresets["kernel_reboot"].Script
	if !strings.Contains(script, "teyir-console") || !strings.Contains(script, "hy2-ops-console") {
		t.Fatalf("kernel_reboot 必须同时匹配 teyir-console 与 hy2-ops-console 容器名，防止误重启面板宿主机")
	}
	if !strings.Contains(script, "teyir-console-api") || !strings.Contains(script, "hy2-ops-console-api") {
		t.Fatalf("kernel_reboot 必须同时检测宿主机 systemd 面板服务，防止裸机部署宿主机被误重启")
	}
	f2b := systemPresets["install_fail2ban"].Script
	if !strings.Contains(f2b, `exit 3`) {
		t.Fatalf("install_fail2ban 包安装失败时应立即非零退出，避免继续写入配置")
	}
	e := &Executor{}
	if _, _, err := e.runPreset(t.Context(), task{parameters: []byte(`{not-json`), allowWrite: true}); err == nil || !strings.Contains(err.Error(), "合法 JSON") {
		t.Fatalf("非法 JSON 参数应直接报格式错误，实际: %v", err)
	}
}
