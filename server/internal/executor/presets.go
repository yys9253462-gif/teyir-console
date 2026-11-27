package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"nodeops/server/internal/sshclient"
)

// 主机预设任务（operation_type = system_preset）。
//
// 参考 kejilion/sh「集群控制」里的批量执行项，但只开放**固定脚本**：
// parameters 里只有 preset 名，脚本内容全部写死在这里，调用方传不进任何命令。
// 「自定义指令」「开放所有端口」刻意不做 —— 前者等于远程任意执行，
// 后者直接拆掉主机防火墙。
//
// 脚本都写成**可重复执行**：已是目标状态的主机直接报告「无需变更」。

type systemPreset struct {
	Label  string
	Script string
	// ReadOnly 的预设只读取系统状态、不做任何改动，不要求主机开启写操作授权。
	ReadOnly bool
}

var systemPresets = map[string]systemPreset{
	"security_audit": {
		Label:    "安全检查",
		ReadOnly: true,
		Script: `score=100; warn(){ echo "[注意] $1"; score=$((score-$2)); }; ok(){ echo "[正常] $1"; }
sshd_val(){ sshd -T 2>/dev/null | awk -v k="$1" '$1==k{print $2; exit}'; }
pa=$(sshd_val passwordauthentication); prl=$(sshd_val permitrootlogin); port=$(sshd_val port)
[ "$pa" = "no" ] && ok "SSH 已禁止密码登录" || warn "SSH 允许密码登录（建议只用密钥）" 20
case "$prl" in no|prohibit-password|without-password) ok "root 不能用密码登录（$prl）";; *) warn "root 可用密码登录（PermitRootLogin=$prl）" 15;; esac
[ "$port" = "22" ] && warn "SSH 使用默认 22 端口，扫描爆破最多" 5 || ok "SSH 端口 $port"
systemctl is-active --quiet fail2ban 2>/dev/null && ok "fail2ban 运行中" || warn "未运行 fail2ban（批量预设任务里可一键安装）" 15
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then ok "ufw 防火墙已启用"
elif command -v nft >/dev/null 2>&1 && [ "$(nft list ruleset 2>/dev/null | grep -c 'drop\|reject')" -gt 0 ]; then ok "nftables 有拦截规则"
elif [ "$(iptables -S 2>/dev/null | grep -c 'DROP\|REJECT')" -gt 0 ]; then ok "iptables 有拦截规则"
else warn "没有发现启用的防火墙规则（云厂商安全组可能在外层拦截）" 10; fi
if command -v apt-get >/dev/null 2>&1; then
  n=$(apt-get -s -o Debug::NoLocking=1 upgrade 2>/dev/null | grep -c '^Inst .*-security')
  [ "$n" -eq 0 ] && ok "没有待装的安全更新" || warn "$n 个安全更新待安装（批量预设任务：更新系统）" 15
  dpkg -s unattended-upgrades >/dev/null 2>&1 && ok "已装 unattended-upgrades 自动安全更新" || warn "未配置自动安全更新" 5
fi
[ -f /var/run/reboot-required ] && warn "有更新需要重启后才生效" 5
users=$(awk -F: '$3==0{print $1}' /etc/passwd | tr '\n' ' '); [ "$users" = "root " ] && ok "只有 root 一个超级用户" || warn "存在多个 UID 0 账号：$users" 15
echo "监听端口：$(ss -tulnH 2>/dev/null | awk '{print $5}' | sed 's/.*://' | sort -un | tr '\n' ' ')"
[ $score -lt 0 ] && score=0
echo "安全评分 $score/100"`,
	},
	"net_route": {
		Label: "回程线路检测",
		Script: `if ! command -v nexttrace >/dev/null 2>&1; then
  echo "安装 nexttrace…"; nt_script=$(mktemp /tmp/nt.XXXXXX) || { echo "创建临时文件失败" >&2; exit 2; }
  trap 'rm -f "$nt_script"' EXIT
  curl -fsSL https://nxtrace.org/nt -o "$nt_script" && bash "$nt_script" >/dev/null 2>&1 || { echo "nexttrace 安装失败" >&2; exit 2; }
fi
classify(){ t="$1"
  if echo "$t" | grep -q "AS4809"; then echo "CN2"; elif echo "$t" | grep -q "AS9929"; then echo "联通 9929"; elif echo "$t" | grep -q "AS10099"; then echo "联通 CUG";
  elif echo "$t" | grep -qE "AS58807|AS58453"; then echo "移动 CMI/CMIN2"; elif echo "$t" | grep -q "AS4837"; then echo "联通 4837";
  elif echo "$t" | grep -q "AS4134"; then echo "电信 163"; elif echo "$t" | grep -q "AS9808"; then echo "移动 CMNET"; else echo "未识别"; fi; }
summary=""
for item in "电信:219.141.136.12" "联通:202.106.50.1" "移动:221.179.155.161"; do
  name=${item%%:*}; ip=${item#*:}
  out=$(timeout 120 nexttrace --no-color -q 1 -g cn "$ip" 2>/dev/null)
  ases=$(echo "$out" | grep -oE "AS[0-9]+" | awk '!s[$0]++' | tr '\n' ' ')
  kind=$(classify "$out")
  echo "$name 回程：$kind　经过 $ases"
  summary="$summary $name:$kind"
done
echo "线路汇总：$summary"`,
	},
	"uninstall_hy2": {
		Label: "卸载 Hysteria 2",
		Script: `if [ ! -f /etc/hysteria/config.yaml ] && ! systemctl list-unit-files 2>/dev/null | grep -q hysteria-server; then echo "这台主机没有安装 Hysteria 2，无需卸载"; exit 0; fi
if [ -x /etc/hysteria/install.sh ] || [ -f /etc/hysteria/install.sh ]; then
  printf 'y\nN\n' | bash /etc/hysteria/install.sh uninstall 2>&1 | tail -n 20
elif [ -n "${HY2_INSTALL_SCRIPT_URL:-}" ]; then
  hy2_script=$(mktemp /tmp/hy2-install.XXXXXX) || { echo "创建临时文件失败" >&2; exit 2; }
  trap 'rm -f "$hy2_script"' EXIT
  curl -fsSL "$HY2_INSTALL_SCRIPT_URL" -o "$hy2_script" || { echo "下载安装脚本失败" >&2; exit 2; }
  printf 'y\nN\n' | bash "$hy2_script" uninstall 2>&1 | tail -n 20
else
  systemctl stop hysteria-server 2>/dev/null || true
  systemctl disable hysteria-server 2>/dev/null || true
  echo "已停用 hysteria-server（主机未缓存 /etc/hysteria/install.sh，如需完整卸载请手动执行对应安装脚本）"
fi
systemctl is-active --quiet hysteria-server 2>/dev/null && { echo "卸载后 hysteria-server 仍在运行" >&2; exit 3; }
echo "已卸载 Hysteria 2；记得在面板「HY2 节点」里删除对应节点"`,
	},
	"update": {
		Label: "更新系统软件包",
		Script: `export DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null 2>&1; then
  apt-get update -q && apt-get -y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold upgrade || { echo "软件包更新失败（常见原因：apt 被自动更新占用锁，稍后重试）" >&2; exit 3; }
elif command -v dnf >/dev/null 2>&1; then
  dnf -y -q upgrade || { echo "软件包更新失败" >&2; exit 3; }
elif command -v yum >/dev/null 2>&1; then
  yum -y -q update || { echo "软件包更新失败" >&2; exit 3; }
else
  echo "未识别的包管理器" >&2; exit 2
fi
echo "---"
if [ -f /var/run/reboot-required ]; then echo "提示：有更新需要重启后生效"; else echo "无需重启"; fi`,
	},
	// 2026-11-01 加。只有确实装上了比运行中更新的内核才重启 —— 内核已是最新时
	// 不打扰节点上的用户。重启用 systemd-run 延后 10 秒，让本次 SSH 先正常返回，
	// 任务记为成功而不是「连接断开」。
	//
	// 面板所在主机（跑着 hy2-ops-console 容器）只升级不重启：重启它等于
	// 面板、商城、告警一起下线，而且这条任务自己也收不到结果。
	"kernel_reboot": {
		Label: "更新内核并重启",
		Script: `export DEBIAN_FRONTEND=noninteractive
running=$(uname -r)
if command -v apt-get >/dev/null 2>&1; then
  # --with-new-pkgs：新内核是新包名（linux-image-6.1.0-NN），普通 upgrade 会把它 held back。
  apt-get update -q && apt-get -y -q --with-new-pkgs -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold upgrade || { echo "内核更新失败，未重启（常见原因：apt 被自动更新占用锁，稍后重试）" >&2; exit 3; }
elif command -v dnf >/dev/null 2>&1; then
  dnf -y -q upgrade 'kernel*' || { echo "内核更新失败，未重启" >&2; exit 3; }
elif command -v yum >/dev/null 2>&1; then
  yum -y -q update 'kernel*' || { echo "内核更新失败，未重启" >&2; exit 3; }
else
  echo "未识别的包管理器" >&2; exit 2
fi
newest=$(ls /boot/vmlinuz-* 2>/dev/null | sed 's|^/boot/vmlinuz-||' | grep -v rescue | sort -V | tail -n 1)
echo "---"
echo "运行中内核：$running；已安装最新：${newest:-未知}"
if [ -z "$newest" ] || [ "$newest" = "$running" ]; then
  if [ -f /var/run/reboot-required ]; then echo "内核已是最新，但有其他更新提示需要重启；本任务只为新内核重启，未重启"
  else echo "内核已是最新（$running），无需重启"; fi
  exit 0
fi
if (command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' 2>/dev/null | grep -Eq '(teyir-console|hy2-ops-console)') || systemctl is-active --quiet teyir-console-api 2>/dev/null || systemctl is-active --quiet hy2-ops-console-api 2>/dev/null; then
  echo "这是面板所在主机：新内核 $newest 已安装但不自动重启，请择时手动 reboot"; exit 0
fi
# 正在运行、却没设开机自启的服务，重启后不会回来 —— 先列出来，便于事后核对。
for svc in $(systemctl list-units --type=service --state=running --no-legend --plain 2>/dev/null | awk '{print $1}'); do
  [ "$(systemctl is-enabled "$svc" 2>/dev/null)" = "disabled" ] && echo "[注意] $svc 正在运行但未设开机自启，重启后不会自动恢复"
done
if command -v systemd-run >/dev/null 2>&1; then
  systemd-run --quiet --on-active=10 /bin/systemctl reboot
else
  setsid nohup sh -c 'sleep 10; reboot' >/dev/null 2>&1 &
fi
echo "新内核 $newest 已安装，10 秒后重启（约 1-2 分钟恢复，期间节点用户会断线）"`,
	},
	"clean": {
		Label: "清理系统垃圾",
		Script: `before=$(df -P / | awk 'NR==2{print $4}')
if command -v apt-get >/dev/null 2>&1; then
  DEBIAN_FRONTEND=noninteractive apt-get -y -q autoremove --purge && apt-get -q clean
elif command -v dnf >/dev/null 2>&1; then
  dnf -y -q autoremove; dnf -q clean all
elif command -v yum >/dev/null 2>&1; then
  yum -y -q autoremove; yum -q clean all
fi
command -v journalctl >/dev/null 2>&1 && journalctl --vacuum-time=7d --vacuum-size=200M >/dev/null 2>&1
find /tmp /var/tmp -xdev -type f -atime +7 -delete 2>/dev/null
after=$(df -P / | awk 'NR==2{print $4}')
echo "释放空间约 $(( (after - before) / 1024 )) MB；根分区剩余 $(( after / 1024 )) MB"`,
	},
	"swap_1g": {
		Label: "设置 1G 虚拟内存",
		Script: `if [ "$(swapon --noheadings 2>/dev/null | wc -l)" -gt 0 ]; then
  echo "已有 swap，无需变更："; swapon --show; exit 0
fi
avail=$(df -P / | awk 'NR==2{print $4}')
if [ "$avail" -lt 2097152 ]; then echo "根分区剩余不足 2G，未创建 swap" >&2; exit 2; fi
fallocate -l 1G /swapfile 2>/dev/null || dd if=/dev/zero of=/swapfile bs=1M count=1024 status=none || { echo "创建 /swapfile 失败" >&2; exit 3; }
# 启用失败就不写 fstab：否则开机时挂一个无效的 swap 条目，而任务却报「已创建」。
chmod 600 /swapfile && mkswap /swapfile >/dev/null && swapon /swapfile || { echo "启用 swap 失败（mkswap / swapon 出错），未写入 /etc/fstab" >&2; rm -f /swapfile; exit 3; }
grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
echo "已创建 1G swap："; swapon --show`,
	},
	"timezone_shanghai": {
		Label: "时区设为 Asia/Shanghai",
		Script: `cur=$(timedatectl show -p Timezone --value 2>/dev/null || cat /etc/timezone 2>/dev/null)
if [ "$cur" = "Asia/Shanghai" ]; then echo "时区已是 Asia/Shanghai，无需变更"; exit 0; fi
if command -v timedatectl >/dev/null 2>&1; then timedatectl set-timezone Asia/Shanghai || { echo "设置时区失败" >&2; exit 3; }
else ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && echo Asia/Shanghai > /etc/timezone || { echo "设置时区失败" >&2; exit 3; }; fi
echo "时区 $cur → Asia/Shanghai；当前时间 $(date '+%F %T %Z')"`,
	},
	"install_fail2ban": {
		Label: "安装 fail2ban（SSH 防爆破）",
		Script: `if systemctl is-active --quiet fail2ban 2>/dev/null; then echo "fail2ban 已在运行，无需变更"; fail2ban-client status sshd 2>/dev/null | tail -n 3; exit 0; fi
if command -v apt-get >/dev/null 2>&1; then DEBIAN_FRONTEND=noninteractive apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get -y -q install fail2ban || { echo "安装 fail2ban 失败" >&2; exit 3; }
elif command -v dnf >/dev/null 2>&1; then dnf -y -q install epel-release; dnf -y -q install fail2ban || { echo "安装 fail2ban 失败" >&2; exit 3; }
elif command -v yum >/dev/null 2>&1; then yum -y -q install epel-release; yum -y -q install fail2ban || { echo "安装 fail2ban 失败" >&2; exit 3; }
else echo "未识别的包管理器" >&2; exit 2; fi
if [ ! -f /etc/fail2ban/jail.local ]; then
  printf '[sshd]\nenabled = true\nmaxretry = 5\nfindtime = 10m\nbantime = 1h\n' > /etc/fail2ban/jail.local
  if ! [ -f /var/log/auth.log ] && ! [ -f /var/log/secure ]; then printf 'backend = systemd\n' >> /etc/fail2ban/jail.local; fi
fi
systemctl enable --now fail2ban && sleep 2 && systemctl is-active fail2ban && echo "fail2ban 已启用（sshd：5 次失败封 1 小时）"`,
	},
}

// PresetNames 供 API 层校验 parameters.preset。
func PresetNames() map[string]string {
	out := make(map[string]string, len(systemPresets))
	for k, v := range systemPresets {
		out[k] = v.Label
	}
	return out
}

func (e *Executor) runPreset(ctx context.Context, t task) (string, []logLine, error) {
	var payload struct {
		Preset string `json:"preset"`
	}
	if len(t.parameters) > 0 {
		if err := json.Unmarshal(t.parameters, &payload); err != nil {
			return "", nil, fmt.Errorf("parameters 不是合法 JSON")
		}
	}
	preset, ok := systemPresets[payload.Preset]
	if !ok {
		return "", nil, fmt.Errorf("未知的预设任务: %q", payload.Preset)
	}
	if !preset.ReadOnly && !t.allowWrite {
		return "", nil, fmt.Errorf("该主机未开启写操作（allow_write_ops=false），预设任务已拒绝")
	}
	client, err := e.dial(ctx, t)
	if err != nil {
		return "", nil, err
	}
	defer client.Close()

	// 脚本经 base64 传给 bash 的 stdin，避免任何引号转义问题；
	// 非 root 用 sudo -n，缺 sudoers 时立即失败而不是卡在密码提示。
	sudo := "sudo -n "
	if strings.TrimSpace(t.user) == "root" {
		sudo = ""
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("set -o pipefail\n" + preset.Script + "\n"))
	// 2026-11-04：在远端只保留最后 400 行再传回来。SSH 输出缓冲保留的是**开头** 256 KiB：
	// 几百个包的 apt update 一超过上限，留下的是中间段，结尾真正的报错从没传回来，
	// 任务摘要只剩一句「输出超过上限已截断」。下面只用最后 60 行，所以截头不丢信息。
	// pipefail 让退出码仍是脚本的，而不是 tail 的。
	// 整条管道放进 bash -c：登录 shell 可能是 dash（不认 pipefail，会直接报错退出）。
	// encoded 只含 base64 字符，单引号包裹是安全的。
	command := fmt.Sprintf("bash -c 'set -o pipefail; echo %s | base64 -d | %sbash 2>&1 | tail -n 400'", encoded, sudo)
	stdout, stderr, exit, err := sshclient.Run(ctx, client, command)
	if err != nil {
		return "", nil, fmt.Errorf("%s执行失败: %w", preset.Label, err)
	}
	out := strings.TrimSpace(sshclient.CleanCommandOutput(stdout))
	// apt 的输出可能上千行，只留最后 60 行进任务日志。
	if lines := strings.Split(out, "\n"); len(lines) > 60 {
		out = strings.Join(lines[len(lines)-60:], "\n")
	}
	logs := []logLine{{"info", "预设任务：" + preset.Label}}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimRight(line, "\r "); line != "" {
			logs = append(logs, logLine{"info", line})
		}
	}
	if s := strings.TrimSpace(sshclient.CleanCommandOutput(stderr)); s != "" {
		logs = append(logs, logLine{"warn", s})
	}
	last := ""
	if lines := strings.Split(out, "\n"); len(lines) > 0 {
		last = strings.TrimSpace(lines[len(lines)-1])
	}
	if exit != 0 {
		return "", logs, fmt.Errorf("%s失败（退出码 %d）：%s", preset.Label, exit, last)
	}
	if last == "" {
		last = "完成"
	}
	return preset.Label + "：" + last, logs, nil
}
