package httpapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"golang.org/x/crypto/ssh"
	"net"
	"net/http"
	"net/url"
	"nodeops/server/internal/portaltls"
	"strings"
	"time"
)

type migrationIPKey struct{}
type migrationRequestKey struct{}

func migrationAwareClient(base *http.Client, ctx context.Context) *http.Client {
	if _, ok := ctx.Value(migrationIPKey{}).(string); !ok {
		return base
	}
	c := *base
	if t, ok := base.Transport.(*http.Transport); ok {
		copy := t.Clone()
		copy.DisableKeepAlives = true
		c.Transport = copy
	}
	return &c
}
func migrationDialIP(ctx context.Context) (string, bool) {
	ip, ok := ctx.Value(migrationIPKey{}).(string)
	p := net.ParseIP(ip)
	return ip, ok && portaltls.PublicIP(p)
}
func (s *Server) nodeMaintenance(ctx context.Context, id string) bool {
	if ctx.Value(migrationRequestKey{}) != nil {
		return false
	}
	if s.db == nil {
		return false
	}
	var busy bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_operation_leases l JOIN hysteria_nodes n ON n.server_id=l.server_id WHERE n.id=$1::uuid)`, id).Scan(&busy)
	return err != nil || busy
}

type migrationTLS struct {
	Cert     string            `json:"cert"`
	Key      string            `json:"key"`
	Config   string            `json:"config"`
	Insecure bool              `json:"insecure"`
	Files    map[string]string `json:"files"`
}

const readMigrationTLS = `import runpy,pathlib,posixpath,re,shlex,json,base64
allow=('/etc/hysteria/','/var/lib/hysteria/','/root/.cache/hysteria/','/root/.acme.sh/','/etc/letsencrypt/','/etc/ssl/')
root=pathlib.Path('/etc/hysteria');m=runpy.run_path(str(root/'portal.py'))
cert,cn,sans,selfsigned=m['_resolve_live_cert'](root)
cert=pathlib.Path(cert);cfg=(root/'config.yaml').read_text()
k=re.search(r'(?m)^\s+key:\s*(.+)$',cfg)
key=pathlib.Path(shlex.split(k.group(1))[0]) if k else cert.with_suffix('.key')
if not key.is_absolute():key=root/key
if not cert.is_file() or not key.is_file():raise RuntimeError('certificate pair not found')
if cert.stat().st_size>262144 or key.stat().st_size>262144:raise RuntimeError('certificate too large')
files={}
for p in [cert,key,cert.with_suffix('.json')]:
 if p.is_file():
  norm=posixpath.normpath(str(p.resolve()))
  if not norm.startswith(allow):raise RuntimeError('certificate path outside migration allowlist')
  if p.stat().st_size>1048576:raise RuntimeError('certificate metadata too large')
  files[norm]=base64.b64encode(p.read_bytes()).decode()
print(json.dumps({'files':files,'cert':base64.b64encode(cert.read_bytes()).decode(),'key':base64.b64encode(key.read_bytes()).decode(),'config':base64.b64encode(cfg.encode()).decode(),'insecure':bool(selfsigned)}))`
const writeMigrationTLS = `import json,base64,pathlib,posixpath,re,os
allow=('/etc/hysteria/','/var/lib/hysteria/','/root/.cache/hysteria/','/root/.acme.sh/','/etc/letsencrypt/','/etc/ssl/')
x=json.load(__import__('sys').stdin);r=pathlib.Path('/etc/hysteria');d=r/'cert';d.mkdir(mode=0o700,exist_ok=True)
for k,name in [('cert','migration.crt'),('key','migration.key')]:
 p=d/name;p.write_bytes(base64.b64decode(x[k]));p.chmod(0o600)
for name,data in x.get('files',{}).items():
 norm=posixpath.normpath(name)
 if norm!=name or '..' in pathlib.PurePosixPath(name).parts or not norm.startswith(allow):raise RuntimeError('certificate path rejected')
 p=pathlib.Path(norm);p.parent.mkdir(parents=True,exist_ok=True);p.write_bytes(base64.b64decode(data));p.chmod(0o600)
cfg=base64.b64decode(x['config']).decode()
if re.search(r'(?m)^tls:\s*$',cfg):
 cfg=re.sub(r'(?m)^(\s+cert:)\s*.+$',r'\1 /etc/hysteria/cert/migration.crt',cfg)
 cfg=re.sub(r'(?m)^(\s+key:)\s*.+$',r'\1 /etc/hysteria/cert/migration.key',cfg)
p=r/'config.migration-original.yaml';p.write_text(cfg);p.chmod(0o600)`

var migrationTLSAllowPrefixes = []string{
	"/etc/hysteria/",
	"/var/lib/hysteria/",
	"/root/.cache/hysteria/",
	"/root/.acme.sh/",
	"/etc/letsencrypt/",
	"/etc/ssl/",
}

// validMigrationTLSPath 在控制面侧复核迁移证书附件路径，防止被攻陷的旧节点返回含 ".." 穿越段的路径覆写新主机任意文件。
func validMigrationTLSPath(raw string) bool {
	if raw == "" || controlChars.MatchString(raw) || strings.Contains(raw, "\\") {
		return false
	}
	allowed := false
	for _, prefix := range migrationTLSAllowPrefixes {
		if strings.HasPrefix(raw, prefix) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return !strings.HasSuffix(raw, "/")
}

func migrationCertificate(ctx context.Context, c *ssh.Client, sudo string) (migrationTLS, error) {
	var data migrationTLS
	out, err := runHostRead(ctx, c, sudo+"python3 -c "+shellQuote(readMigrationTLS))
	if err != nil {
		return data, err
	}
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &data) != nil {
		return data, errors.New("旧节点证书元数据无法解析")
	}
	for pathKey := range data.Files {
		if !validMigrationTLSPath(pathKey) {
			return data, errors.New("旧节点返回的证书附件路径不在安全白名单内")
		}
	}
	cert, err := base64.StdEncoding.DecodeString(data.Cert)
	if err != nil {
		return data, err
	}
	key, err := base64.StdEncoding.DecodeString(data.Key)
	if err != nil {
		return data, err
	}
	if _, err = tls.X509KeyPair(cert, key); err != nil {
		return data, errors.New("旧节点证书与私钥不匹配")
	}
	block, _ := pem.Decode(cert)
	if block == nil {
		return data, errors.New("旧节点证书不是PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return data, err
	}
	if leaf.NotAfter.Before(time.Now().Add(time.Hour)) {
		return data, errors.New("旧节点证书将在1小时内过期，请先续证再迁移")
	}
	return data, nil
}
func transferMigrationTLS(ctx context.Context, c *ssh.Client, sudo string, t migrationTLS) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return runWithStdin(ctx, c, sudo+"python3 -c "+shellQuote(writeMigrationTLS), raw)
}

func migrationEndpoint(base, newHost, domain string) (string, error) {
	// 新的接入始终使用业务域名，避免迁移后仍连旧IP。
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	{
		port := u.Port()
		if port != "" {
			u.Host = net.JoinHostPort(domain, port)
		} else {
			u.Host = domain
		}
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}
