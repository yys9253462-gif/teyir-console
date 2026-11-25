package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"nodeops/server/internal/sshclient"
)

const terminalCookieName = "nodeops_terminal"
const previewMaxBytes = 256 << 10
const terminalMaxMessage = 16 << 10
const terminalDuration = 30 * time.Minute

// hostSSH requires an enrolled fingerprint; privileged host operations never
// silently trust a previously unseen key. Service write-unit policy is unrelated.
func (s *Server) hostSSH(ctx context.Context, w http.ResponseWriter, r *http.Request) *ssh.Client {
	var host, user, pin, kind string
	var port int
	var encrypted []byte
	err := s.db.QueryRow(ctx, `SELECT s.host,s.ssh_port,s.ssh_user,COALESCE(s.host_fingerprint,''),COALESCE(sc.auth_type,''),sc.ciphertext FROM servers s LEFT JOIN LATERAL (SELECT auth_type,ciphertext FROM server_credentials WHERE server_id=s.id ORDER BY created_at DESC LIMIT 1) sc ON true WHERE s.id=$1::uuid AND s.deleted_at IS NULL`, r.PathValue("id")).Scan(&host, &port, &user, &pin, &kind, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		hostError(w, 404, "主机不存在或已删除")
		return nil
	}
	if err != nil {
		s.databaseError(w, r, err)
		return nil
	}
	if len(encrypted) == 0 || s.box == nil {
		hostError(w, 409, "主机未配置可用 SSH 凭据")
		return nil
	}
	if pin == "" {
		hostError(w, 409, "主机尚未登记 SSH 指纹，请先完成连接测试")
		return nil
	}
	secret, err := s.box.Open(encrypted)
	if err != nil {
		hostError(w, 500, "SSH 凭据解密失败")
		return nil
	}
	defer clear(secret)
	if kind == "" {
		kind = "ssh_key"
	}
	client, _, err := sshclient.Dial(ctx, host, port, user, sshclient.Credential{Type: kind, Secret: secret}, pin)
	if err != nil {
		// 2026-11-06：指纹不符要和「连不上」分开（与 testServerSSH 同一处理）。原来文件浏览 / 终端 /
		// 容器列表只回一句「SSH 连接失败」，运维看不出是重装还是网络断，最可能的下一步是去改主机地址 ——
		// 而改地址恰恰会清空 host_fingerprint，等于用「改配置」绕过了 pin 确认流程。
		if m := fingerprintMismatch(err); m != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok": false, "fingerprint_mismatch": true, "expected": m.Expected, "got": m.Got,
				"error": "SSH 主机指纹变了：如果你刚重装过系统，这是正常的，在主机页确认新指纹即可（不要靠改地址绕过）；否则可能存在中间人攻击",
			})
			return nil
		}
		hostError(w, 502, "SSH 连接失败: "+err.Error())
		return nil
	}
	return client
}
func hostError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func (s *Server) hostReadAllowed(w http.ResponseWriter, r *http.Request) bool {
	c, _ := claimsFrom(r.Context())
	return s.enforceReadLimit(w, c.Subject)
}

// Only the requested presentation fields leave the host, never inspect/env data.
const dockerListCommand = `docker ps -a --no-trunc --format '{"id":{{json .ID}},"name":{{json .Names}},"image":{{json .Image}},"state":{{json .State}},"status":{{json .Status}},"ports":{{json .Ports}}}'`
const composeListCommand = `docker compose ls --all --format json`

type containerRow struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
	Status string `json:"status"`
	Ports  string `json:"ports"`
}
type composeRow struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	ConfigFiles string `json:"config_files"`
}

func parseContainers(raw string) ([]containerRow, error) {
	rows := make([]containerRow, 0)
	dec := json.NewDecoder(strings.NewReader(raw))
	for {
		var row containerRow
		err := dec.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if row.ID == "" {
			return nil, errors.New("missing container ID")
		}
		rows = append(rows, row)
	}
	return rows, nil
}
func parseCompose(raw string) ([]composeRow, error) {
	var records []struct {
		Name        string
		Status      string
		ConfigFiles string
	}
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		return nil, err
	}
	if records == nil {
		return nil, errors.New("expected Compose project array")
	}
	rows := make([]composeRow, 0, len(records))
	for _, v := range records {
		if v.Name == "" {
			return nil, errors.New("missing Compose project name")
		}
		rows = append(rows, composeRow{v.Name, v.Status, v.ConfigFiles})
	}
	return rows, nil
}

// boundedBuffer aborts malicious/unbounded remote output rather than allocating
// arbitrarily large buffers. Used instead of the generic SSH command helper.
type boundedBuffer struct {
	strings.Builder
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("remote output exceeds limit")
	}
	return b.Builder.Write(p)
}
func runHostRead(ctx context.Context, client *ssh.Client, command string, limits ...int) (string, error) {
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	limit := 2 << 20
	if len(limits) > 0 && limits[0] > 0 && limits[0] <= 32<<20 {
		limit = limits[0]
	}
	out := &boundedBuffer{limit: limit}
	stderr := &boundedBuffer{limit: 16 << 10}
	session.Stdout = out
	session.Stderr = stderr
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case <-ctx.Done():
		client.Close()
		return "", ctx.Err()
	case err = <-done:
	}
	if err != nil {
		return "", fmt.Errorf("远端命令失败: %s", truncateRunes(strings.TrimSpace(stderr.String()), 300))
	}
	return out.String(), nil
}
func (s *Server) dockerContainers(w http.ResponseWriter, r *http.Request) { s.dockerList(w, r, false) }
func (s *Server) dockerCompose(w http.ResponseWriter, r *http.Request)    { s.dockerList(w, r, true) }
func (s *Server) dockerList(w http.ResponseWriter, r *http.Request, compose bool) {
	if !s.hostReadAllowed(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	client := s.hostSSH(ctx, w, r)
	if client == nil {
		return
	}
	defer client.Close()
	cmd, key := dockerListCommand, "containers"
	if compose {
		cmd, key = composeListCommand, "projects"
	}
	raw, err := runHostRead(ctx, client, cmd)
	if err != nil {
		hostError(w, 502, err.Error())
		return
	}
	var rows any
	if compose {
		rows, err = parseCompose(raw)
	} else {
		rows, err = parseContainers(raw)
	}
	if err != nil {
		hostError(w, 502, "Docker 返回的数据格式无效")
		return
	}
	s.audit(ctx, r, "host_"+key+"_read", "server", r.PathValue("id"), nil)
	writeJSON(w, 200, map[string]any{key: rows})
}
func remotePath(value string) (string, error) {
	if value == "" {
		return "/", nil
	}
	if !path.IsAbs(value) || strings.ContainsRune(value, 0) || len(value) > 4096 {
		return "", errors.New("需要长度不超过4096的绝对路径")
	}
	return path.Clean(value), nil
}

type remoteEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	Mode       string    `json:"mode"`
	ModifiedAt time.Time `json:"modified_at"`
}

func (s *Server) remoteFiles(w http.ResponseWriter, r *http.Request) { s.remoteFileRead(w, r, false) }
func (s *Server) remoteFilePreview(w http.ResponseWriter, r *http.Request) {
	s.remoteFileRead(w, r, true)
}
func (s *Server) remoteFileRead(w http.ResponseWriter, r *http.Request, preview bool) {
	if !s.hostReadAllowed(w, r) {
		return
	}
	p, err := remotePath(r.URL.Query().Get("path"))
	if err != nil {
		hostError(w, 400, err.Error())
		return
	}
	if preview && r.URL.Query().Get("path") == "" {
		hostError(w, 400, "预览需要指定文件绝对路径")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	client := s.hostSSH(ctx, w, r)
	if client == nil {
		return
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	if !preview {
		entries, truncated, readErr := readBoundedDirectory(client, p)
		if readErr != nil {
			fileReadError(w, readErr)
			return
		}
		s.audit(ctx, r, "host_directory_read", "server", r.PathValue("id"), map[string]any{"path": p})
		writeJSON(w, 200, map[string]any{"path": p, "entries": entries, "truncated": truncated})
		return
	}
	fs, err := sftp.NewClient(client)
	if err != nil {
		hostError(w, 502, "SFTP 不可用: "+err.Error())
		return
	}
	defer fs.Close()
	w.Header().Set("Cache-Control", "no-store")
	// Lstat disallows device/FIFO/symlink previews; Fstat rechecks the actual handle.
	info, err := fs.Lstat(p)
	if err != nil {
		fileReadError(w, err)
		return
	}
	if !info.Mode().IsRegular() {
		hostError(w, 415, "仅支持普通文本文件，不支持目录、链接或设备")
		return
	}
	if info.Size() > previewMaxBytes {
		hostError(w, 413, "文件超过256 KiB预览上限")
		return
	}
	file, err := fs.Open(p)
	if err != nil {
		fileReadError(w, err)
		return
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		fileReadError(w, err)
		return
	}
	if !info.Mode().IsRegular() {
		hostError(w, 415, "仅支持普通文本文件")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, previewMaxBytes+1))
	if err != nil {
		fileReadError(w, err)
		return
	}
	if len(data) > previewMaxBytes {
		hostError(w, 413, "文件超过256 KiB预览上限")
		return
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		hostError(w, 415, "文件不是 UTF-8 文本或包含二进制数据")
		return
	}
	s.audit(ctx, r, "host_file_preview", "server", r.PathValue("id"), map[string]any{"path": p, "bytes": len(data)})
	writeJSON(w, 200, map[string]any{"path": p, "content": string(data), "size": len(data), "truncated": false, "max_bytes": previewMaxBytes})
}

// SFTP v3 directory streaming: at most 1MiB per packet, 2001 entries
// and 128 READDIR requests. Unlike Client.ReadDir it never aggregates a whole
// remote directory before applying the limit.
type sftpCursor struct {
	data []byte
	err  error
}

func (c *sftpCursor) take(n int) []byte {
	if n < 0 || n > len(c.data) {
		c.err = io.ErrUnexpectedEOF
		return nil
	}
	v := c.data[:n]
	c.data = c.data[n:]
	return v
}
func (c *sftpCursor) u32() uint32 {
	b := c.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}
func (c *sftpCursor) u64() uint64 {
	b := c.take(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}
func (c *sftpCursor) str() string {
	n := c.u32()
	if n > 1<<20 {
		c.err = errors.New("SFTP string too large")
		return ""
	}
	return string(c.take(int(n)))
}
func readSFTPPacket(r io.Reader) (byte, []byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n < 1 || n > 1<<20 {
		return 0, nil, errors.New("SFTP packet exceeds 1MiB")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}
func writeSFTPPacket(w io.Writer, kind byte, data []byte) error {
	buf := make([]byte, 5+len(data))
	binary.BigEndian.PutUint32(buf, uint32(1+len(data)))
	buf[4] = kind
	copy(buf[5:], data)
	_, err := w.Write(buf)
	return err
}
func readBoundedDirectory(client *ssh.Client, p string) ([]remoteEntry, bool, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, false, err
	}
	defer session.Close()
	input, err := session.StdinPipe()
	if err != nil {
		return nil, false, err
	}
	output, err := session.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err = session.RequestSubsystem("sftp"); err != nil {
		return nil, false, err
	}
	if err = writeSFTPPacket(input, 1, ssh.Marshal(struct{ Version uint32 }{3})); err != nil {
		return nil, false, err
	}
	kind, data, err := readSFTPPacket(output)
	if err != nil {
		return nil, false, err
	}
	if kind != 2 || len(data) < 4 || binary.BigEndian.Uint32(data[:4]) != 3 {
		return nil, false, errors.New("SFTP v3 unavailable")
	}
	if err = writeSFTPPacket(input, 11, ssh.Marshal(struct {
		ID   uint32
		Path string
	}{1, p})); err != nil {
		return nil, false, err
	}
	kind, data, err = readSFTPPacket(output)
	if err != nil {
		return nil, false, err
	}
	c := sftpCursor{data: data}
	if c.u32() != 1 {
		return nil, false, errors.New("SFTP request ID mismatch")
	}
	if kind == 101 {
		return nil, false, sftpStatusError(&c)
	}
	if kind != 102 {
		return nil, false, errors.New("SFTP directory handle unavailable")
	}
	handle := c.str()
	if c.err != nil {
		return nil, false, c.err
	}
	defer func() {
		writeSFTPPacket(input, 4, ssh.Marshal(struct {
			ID     uint32
			Handle string
		}{999, handle}))
	}()
	// 初始容量取 64 按需扩容，避免浏览空目录或小目录时每次预分配 2000 项大块内存。
	entries := make([]remoteEntry, 0, 64)
	for id := uint32(2); id < 130; id++ {
		if err = writeSFTPPacket(input, 12, ssh.Marshal(struct {
			ID     uint32
			Handle string
		}{id, handle})); err != nil {
			return nil, false, err
		}
		kind, data, err = readSFTPPacket(output)
		if err != nil {
			return nil, false, err
		}
		c = sftpCursor{data: data}
		if c.u32() != id {
			return nil, false, errors.New("SFTP request ID mismatch")
		}
		if kind == 101 {
			code := c.u32()
			if code == 1 {
				return sortedDirectory(entries), false, nil
			}
			return nil, false, fmt.Errorf("SFTP directory status %d", code)
		}
		if kind != 104 {
			return nil, false, errors.New("SFTP expected NAME packet")
		}
		count := c.u32()
		if count > 20000 {
			return nil, false, errors.New("SFTP excessive directory packet count")
		}
		for i := uint32(0); i < count; i++ {
			name := c.str()
			c.str()
			flags := c.u32()
			var size uint64
			var mode, modified uint32
			if flags&1 != 0 {
				size = c.u64()
			}
			if flags&2 != 0 {
				c.u32()
				c.u32()
			}
			if flags&4 != 0 {
				mode = c.u32()
			}
			if flags&8 != 0 {
				c.u32()
				modified = c.u32()
			}
			if flags&0x80000000 != 0 {
				n := c.u32()
				if n > 1000 {
					return nil, false, errors.New("SFTP excessive extended attributes")
				}
				for j := uint32(0); j < n; j++ {
					c.str()
					c.str()
				}
			}
			if c.err != nil {
				return nil, false, c.err
			}
			if name == "." || name == ".." {
				continue
			}
			if name == "" || strings.ContainsAny(name, "/\x00") || !utf8.ValidString(name) {
				return nil, false, errors.New("SFTP invalid entry name")
			}
			if len(entries) >= 2000 {
				return sortedDirectory(entries), true, nil
			}
			typ := "file"
			switch mode & 0170000 {
			case 0040000:
				typ = "directory"
			case 0120000:
				typ = "symlink"
			case 0100000:
			default:
				typ = "other"
			}
			entries = append(entries, remoteEntry{name, path.Join(p, name), typ, int64(size), fmt.Sprintf("%04o", mode&07777), time.Unix(int64(modified), 0)})
		}
	}
	return sortedDirectory(entries), true, nil
}
func sortedDirectory(entries []remoteEntry) []remoteEntry {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}
func sftpStatusError(c *sftpCursor) error {
	code := c.u32()
	message := c.str()
	if c.err != nil {
		return c.err
	}
	switch code {
	case 2:
		return os.ErrNotExist
	case 3:
		return os.ErrPermission
	}
	return fmt.Errorf("SFTP status %d: %s", code, truncateRunes(message, 200))
}

func fileReadError(w http.ResponseWriter, err error) {
	status := 502
	if os.IsNotExist(err) {
		status = 404
	} else if os.IsPermission(err) {
		status = 403
	}
	hostError(w, status, "远端文件读取失败: "+err.Error())
}

// The cookie is confined to terminal authentication; normal REST remains Bearer.
func (s *Server) terminalCookie(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || token == r.Header.Get("Authorization") {
		hostError(w, 401, "需要 Bearer 会话")
		return
	}
	claims, ok := claimsFrom(r.Context())
	if !ok {
		hostError(w, 401, "无效会话")
		return
	}
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	expires := terminalExpiry(claims.Expires)
	seconds := int(time.Until(expires).Seconds())
	if seconds < 1 {
		hostError(w, 401, "会话即将或已经过期")
		return
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{Name: terminalCookieName, Value: token, Path: "/api/servers/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: seconds, Expires: expires})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]bool{"ready": true})
}
func terminalExpiry(exp int64) time.Time {
	deadline := time.Now().Add(terminalDuration)
	if jwt := time.Unix(exp, 0); jwt.Before(deadline) {
		deadline = jwt
	}
	return deadline
}
func (s *Server) clearTerminalCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: terminalCookieName, Value: "", Path: "/api/servers/", HttpOnly: true, Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"), SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]bool{"ready": false})
}
func terminalOriginAllowed(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return origin.Scheme == scheme && strings.EqualFold(origin.Host, r.Host)
}
func terminalQueryAllowed(r *http.Request) bool {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false
	}
	for key, entries := range values {
		if len(entries) != 1 {
			return false
		}
		if key != "cols" && key != "rows" {
			return false
		}
	}
	return true
}
func (s *Server) terminalAuthenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !terminalQueryAllowed(r) {
			hostError(w, 400, "终端 URL 仅允许 cols/rows，禁止认证参数")
			return
		}
		if !terminalOriginAllowed(r) {
			hostError(w, 403, "终端需要同源 Origin")
			return
		}
		cookie, err := r.Cookie(terminalCookieName)
		if err != nil || cookie.Value == "" {
			hostError(w, 401, "请先建立终端会话")
			return
		}
		cloned := r.Clone(r.Context())
		cloned.Header = r.Header.Clone()
		cloned.Header.Set("Authorization", "Bearer "+cookie.Value)
		s.authenticated(next)(w, cloned)
	}
}

type terminalSessionGate struct {
	mu           sync.Mutex
	total        int
	users, hosts map[string]int
}

func (g *terminalSessionGate) acquire(user, host string) (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.total >= 16 || g.users[user] >= 2 || g.hosts[host] >= 2 {
		return nil, false
	}
	if g.users == nil {
		g.users = map[string]int{}
		g.hosts = map[string]int{}
	}
	g.total++
	g.users[user]++
	g.hosts[host]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.total--
			g.users[user]--
			g.hosts[host]--
			if g.users[user] == 0 {
				delete(g.users, user)
			}
			if g.hosts[host] == 0 {
				delete(g.hosts, host)
			}
		})
	}, true
}

type terminalMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

func terminalDimensions(cols, rows int) bool {
	return cols >= 1 && cols <= 500 && rows >= 1 && rows <= 200
}
func (s *Server) remoteTerminal(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		hostError(w, http.StatusBadRequest, "终端需要显式 WebSocket 连接")
		return
	}
	cols, rows := 80, 24
	var err error
	if v := r.URL.Query().Get("cols"); v != "" {
		cols, err = strconv.Atoi(v)
		if err != nil {
			hostError(w, 400, "cols 无效")
			return
		}
	}
	if v := r.URL.Query().Get("rows"); v != "" {
		rows, err = strconv.Atoi(v)
		if err != nil {
			hostError(w, 400, "rows 无效")
			return
		}
	}
	if !terminalDimensions(cols, rows) {
		hostError(w, 400, "终端尺寸须为 cols 1-500 / rows 1-200")
		return
	}
	if !s.hostReadAllowed(w, r) {
		return
	}
	claims, _ := claimsFrom(r.Context())
	hostID := r.PathValue("id")
	if norm, ok := canonicalUUID(hostID); ok {
		hostID = norm
	}
	release, ok := s.terminalGate.acquire(claims.Subject, hostID)
	if !ok {
		hostError(w, 429, "终端并发上限：每用户/主机2会话，全局16")
		return
	}
	defer release()
	deadline := terminalExpiry(claims.Expires)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	client := s.hostSSH(ctx, w, r)
	if client == nil {
		return
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		hostError(w, 502, "SSH 会话不可用")
		return
	}
	defer session.Close()
	stdin, err := session.StdinPipe()
	if err != nil {
		hostError(w, 502, "终端输入不可用")
		return
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	session.Stdout = writer
	session.Stderr = writer
	if err = session.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}); err != nil {
		hostError(w, 502, "SSH PTY 不可用")
		return
	}
	// Upgrade before starting a shell: a normal HTTP GET cannot start a terminal.
	upgrader := websocket.Upgrader{CheckOrigin: terminalOriginAllowed, HandshakeTimeout: 10 * time.Second}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	closeWS := context.AfterFunc(ctx, func() { ws.Close() })
	defer closeWS()
	var writeMu sync.Mutex
	send := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return ws.WriteJSON(value)
	}
	if err = session.Shell(); err != nil {
		send(map[string]any{"type": "error", "error": "SSH shell 启动失败"})
		return
	}
	started := time.Now()
	s.audit(ctx, r, "host_terminal_connect", "server", r.PathValue("id"), nil)
	defer func() {
		auditCtx, end := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		defer end()
		s.audit(auditCtx, r, "host_terminal_disconnect", "server", r.PathValue("id"), map[string]any{"duration_seconds": int(time.Since(started).Seconds())})
	}()
	// 下发真实剩余会话秒数（受登录令牌剩余有效期和单次 30 分钟上限共同约束），避免前端倒计时失真。
	maxSessionSeconds := int(time.Until(deadline).Seconds())
	if maxSessionSeconds < 1 {
		maxSessionSeconds = 1
	}
	if err = send(map[string]any{"type": "ready", "max_session_seconds": maxSessionSeconds}); err != nil {
		return
	}
	ws.SetReadLimit(terminalMaxMessage)
	ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		check := time.NewTicker(30 * time.Second)
		defer check.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ping.C:
				if ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					cancel()
					return
				}
			case <-check.C:
				verifyCtx, end := context.WithTimeout(ctx, 5*time.Second)
				current, verifyErr := s.authenticate(r.Clone(verifyCtx))
				end()
				// 2026-11-06：数据库暂时不可用时跳过这一轮复核，不断开正在用的终端（下一轮再核）。
				var unavailable errSessionCheckUnavailable
				if errors.As(verifyErr, &unavailable) {
					continue
				}
				if verifyErr != nil || current.Role != "admin" {
					send(map[string]any{"type": "error", "error": "会话已撤销、过期或管理员权限已变更"})
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-heartbeatDone }()
	done := make(chan struct{})
	waitDone := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				if send(map[string]any{"type": "output", "encoding": "base64", "data": base64.StdEncoding.EncodeToString(buf[:n])}) != nil {
					ws.Close()
					return
				}
			}
			if readErr != nil {
				send(map[string]any{"type": "exit"})
				ws.Close()
				return
			}
		}
	}()
	go func() { defer close(waitDone); session.Wait(); writer.Close() }()
	defer func() {
		// 先取消上下文立即唤醒心跳协程退出，再依次关闭连接与等待子协程收尾。
		cancel()
		client.Close()
		reader.Close()
		writer.Close()
		ws.Close()
		<-done
		<-waitDone
	}()
	for {
		kind, data, readErr := ws.ReadMessage()
		if readErr != nil {
			return
		}
		if kind != websocket.TextMessage {
			send(map[string]any{"type": "error", "error": "仅支持 JSON 文本消息"})
			return
		}
		var msg terminalMessage
		if json.Unmarshal(data, &msg) != nil {
			send(map[string]any{"type": "error", "error": "无效 JSON 消息"})
			return
		}
		switch msg.Type {
		case "input":
			if len(msg.Data) > terminalMaxMessage || !utf8.ValidString(msg.Data) {
				send(map[string]any{"type": "error", "error": "输入过大或编码无效"})
				return
			}
			if _, err = io.WriteString(stdin, msg.Data); err != nil {
				return
			}
		case "resize":
			if !terminalDimensions(msg.Cols, msg.Rows) {
				send(map[string]any{"type": "error", "error": "终端尺寸无效"})
				return
			}
			if err = session.WindowChange(msg.Rows, msg.Cols); err != nil {
				return
			}
		default:
			send(map[string]any{"type": "error", "error": "未知终端消息类型"})
			return
		}
	}
}
