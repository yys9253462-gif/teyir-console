package sshclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Credential carries one decrypted host credential.
type Credential struct {
	Type   string // ssh_key | password
	Secret []byte
}

// Dial opens an SSH connection using the given credential.
// When pinned is non-empty the server host key must match it, otherwise the
// observed fingerprint is returned so the caller can store it on first use.
func Dial(ctx context.Context, host string, port int, user string, cred Credential, pinned string) (*ssh.Client, string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, "", errors.New("ssh: empty host")
	}
	user = strings.TrimSpace(user)
	if user == "" {
		return nil, "", errors.New("ssh: empty user")
	}
	if port < 1 || port > 65535 {
		return nil, "", fmt.Errorf("ssh: invalid port %d", port)
	}
	var auth []ssh.AuthMethod
	switch cred.Type {
	case "password":
		if len(cred.Secret) == 0 {
			return nil, "", errors.New("ssh: empty password credential")
		}
		auth = []ssh.AuthMethod{ssh.Password(string(cred.Secret))}
	case "ssh_key", "":
		signer, err := keySigner(cred.Secret)
		if err != nil {
			return nil, "", err
		}
		auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	default:
		return nil, "", fmt.Errorf("ssh: unsupported credential type %q", cred.Type)
	}
	// 用互斥锁保护指纹状态，并在首次连接（TOFU）握手后锁定该指纹，
	// 防止长连接发生密钥重协商（rekey）时对端静默更换主机密钥。
	var fpMu sync.Mutex
	expected := strings.TrimSpace(pinned)
	var got string
	config := &ssh.ClientConfig{
		User: user,
		Auth: auth,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := Fingerprint(key)
			fpMu.Lock()
			defer fpMu.Unlock()
			got = fp
			if expected != "" && expected != fp {
				return fmt.Errorf("host key mismatch: expected %s, got %s", expected, fp)
			}
			if expected == "" {
				expected = fp
			}
			return nil
		},
		Timeout:        10 * time.Second,
		BannerCallback: func(string) error { return nil },
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, "", err
	}
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, "", err
	}
	// 上下文取消时立即压低连接截止时间，打断阻塞中的 SSH 握手而非苦等 10 秒超时。
	// 当 stopCancel() 返回 false 说明回调已被调度，必须等待其结束再清除或关闭连接，
	// 彻底消除 SetDeadline(time.Time{}) 与取消回调之间的并发时序竞态。
	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		defer close(cancelDone)
		_ = conn.SetDeadline(time.Now())
	})
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, conn.RemoteAddr().String(), config)
	canceled := !stopCancel()
	if canceled {
		<-cancelDone
	}
	fpMu.Lock()
	observed := got
	fpMu.Unlock()
	if err != nil {
		conn.Close()
		// 指纹不符单独成型：调用方要据此提示「主机密钥变了（多半是重装系统）」，
		// 并允许操作者确认新指纹，而不是只给一句握手失败。
		if pinned != "" && observed != "" && observed != strings.TrimSpace(pinned) {
			return nil, observed, &HostKeyMismatchError{Expected: strings.TrimSpace(pinned), Got: observed}
		}
		return nil, observed, err
	}
	if canceled {
		_ = sshConn.Close()
		return nil, observed, ctx.Err()
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(sshConn, chans, reqs), observed, nil
}

// ValidatePrivateKey 用与连接时相同的解析器检查一把私钥能不能用（上传凭据时调用）。
func ValidatePrivateKey(keyPEM []byte) error {
	_, err := keySigner(keyPEM)
	return err
}

func keySigner(keyPEM []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(keyPEM)
	if err == nil {
		return signer, nil
	}
	// 优先基于解析器返回的类型化错误识别加密私钥（同时覆盖新版 OpenSSH 加密格式与旧版 PEM 加密头），
	// 避免用泛化子串误杀注释中碰巧含 ENCRYPTED 的普通私钥。
	var passErr *ssh.PassphraseMissingError
	if errors.As(err, &passErr) ||
		bytes.Contains(keyPEM, []byte("BEGIN ENCRYPTED PRIVATE KEY")) ||
		bytes.Contains(keyPEM, []byte("Proc-Type: 4,ENCRYPTED")) {
		return nil, errors.New("passphrase-protected keys are not supported")
	}
	return nil, err
}

// Fingerprint renders an OpenSSH-style SHA256 host key fingerprint.
func Fingerprint(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + base64.StdEncoding.EncodeToString(sum[:])
}

// cappedBuffer 是有上限的输出缓冲。
//
// 为什么需要上限（2026-10-30）：Run 的 stdout/stderr 是**无界** bytes.Buffer，
// 而这里执行的是运维脚本 —— 一条写错的重定向（`cat` 大文件、日志滚屏、
// 递归 ls）就能让采集器的内存跟着输出涨。采集器每 30 秒对每台机器跑一遍，
// 一台机器放出一份大输出就足以把整个进程推到 OOM，而 OOM 之后**所有**主机
// 都停止采集 —— 一台机器的错误变成全站故障。
//
// 超限之后继续"读并丢弃"而不是中断命令：中断会让命令以非零退出码结束，
// 上层会把它当成"探测失败"，从而把一个数据问题误诊成节点故障。
type cappedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
	cut   bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - b.buf.Len(); room < len(p) {
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.cut = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.cut {
		return b.buf.String()
	}
	return b.buf.String() + "\n…（输出超过上限已截断）"
}

// maxCommandOutput 单条命令保留的输出上限。采集脚本的正常输出是几百字节到
// 几 KB；256 KiB 足够容纳异常输出又不至于让内存失控。
const maxCommandOutput = 256 << 10

// openSession 在受上下文约束的前提下开启 SSH 会话，防止远端网络黑洞时
// client.NewSession 永久阻塞且无视超时取消。
func openSession(ctx context.Context, client *ssh.Client) (*ssh.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		session *ssh.Session
		err     error
	}
	ch := make(chan result)
	abandoned := make(chan struct{})
	go func() {
		s, err := client.NewSession()
		select {
		case ch <- result{session: s, err: err}:
		case <-abandoned:
			// 主协程已因超时/取消返回，此处直接回收迟到的会话避免通道泄漏。
			if s != nil {
				_ = s.Close()
			}
		}
	}()
	select {
	case r := <-ch:
		return r.session, r.err
	case <-ctx.Done():
		close(abandoned)
		return nil, ctx.Err()
	}
}

// Run executes a command and returns stdout, stderr and the exit code.
func Run(ctx context.Context, client *ssh.Client, command string) (string, string, int, error) {
	session, err := openSession(ctx, client)
	if err != nil {
		return "", "", -1, err
	}
	defer session.Close()

	stdout := &cappedBuffer{limit: maxCommandOutput}
	stderr := &cappedBuffer{limit: maxCommandOutput}
	session.Stdout = stdout
	session.Stderr = stderr

	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		// ⚠️ 2026-10-30：必须等 session.Run 真正返回再读缓冲。
		//
		// 原来是发完 SIGKILL 立刻 `return stdout.String()` —— 而写缓冲的
		// 是**另一个 goroutine**（ssh 会话的读循环），它没有停。
		// 于是：① 是一次真实的数据竞争（bytes.Buffer 不是并发安全的），
		// 在 -race 下必报；② 更糟的是 defer session.Close() 与那个 goroutine
		// 同时操作同一连接，会 panic 或让缓冲区写入悬空 —— 表现为采集器
		// 偶发崩溃，日志里却只有一条"采集主机时 panic"。
		// 等待有限时间；若命令仍未退出，缓冲自身的锁保护并发读取。
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return stdout.String(), stderr.String(), -1, ctx.Err()
	case err := <-done:
		exit := 0
		if err != nil {
			var exitErr *ssh.ExitError
			if errors.As(err, &exitErr) {
				exit = exitErr.ExitStatus()
			} else {
				return stdout.String(), stderr.String(), -1, err
			}
		}
		return stdout.String(), stderr.String(), exit, nil
	}
}

// CleanCommandOutput trims noise so single-line probes can be compared.
func CleanCommandOutput(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r", ""))
}

// HostKeyMismatchError 表示对端主机密钥与登记的指纹不一致。
// 常见原因是重装系统；也可能是中间人攻击 —— 所以绝不自动接受新指纹。
type HostKeyMismatchError struct {
	Expected string
	Got      string
}

func (e *HostKeyMismatchError) Error() string {
	return fmt.Sprintf("host key mismatch: expected %s, got %s", e.Expected, e.Got)
}

// PortProbeCommand 生成「端口是否在监听」的探测命令（POSIX sh，不依赖登录 shell 是 bash）。
//
// 缺 ss 时以 127 退出；ss 自身失败时以 ss 的退出码退出 —— 都不能被 wc 的 0 掩盖。
// 采集器与运维任务的健康检查共用（2026-11-04 从 collector 移来）。
func PortProbeCommand(port int) string {
	return `command -v ss >/dev/null 2>&1 || { echo "ss not found" >&2; exit 127; }; ` +
		`out=$(ss -lunH "sport = :` + strconv.Itoa(port) + `") || exit $?; ` +
		`printf '%s\n' "$out" | grep -c .`
}

// ParsePortProbe 把探测输出解释为「是否在监听」。
//
// grep -c 在零行时退出 1，那是确定的「没在听」；其余非 0 退出码、或输出不是单个整数，
// 都表示探测本身没完成 —— 不能当成「未监听」。
func ParsePortProbe(stdout string, exitCode int) (bool, error) {
	if exitCode != 0 && exitCode != 1 {
		return false, fmt.Errorf("端口探测命令退出码 %d", exitCode)
	}
	n, err := strconv.Atoi(strings.TrimSpace(CleanCommandOutput(stdout)))
	if err != nil || n < 0 {
		return false, fmt.Errorf("端口探测输出无法解析: %.80q", strings.TrimSpace(stdout))
	}
	if exitCode == 1 && n != 0 {
		return false, fmt.Errorf("端口探测结果不一致（退出码 1，输出 %d）", n)
	}
	return n > 0, nil
}
