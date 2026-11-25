package httpapi

import (
	"context"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"

	"nodeops/server/internal/sshclient"
)

// AWS / 部分云镜像会在 root 的 authorized_keys 里写 command="echo Please login as the user \"admin\" ..."：
// 认证能通过，但任何命令都只打印这句话。识别出来就自动改用它要求的用户（同一把私钥）。
var forcedUserPattern = regexp.MustCompile(`login as the user "?([a-z_][a-z0-9._-]{0,31})"?`)

func forcedLoginUser(ctx context.Context, client *ssh.Client) string {
	out, _, _, err := sshclient.Run(ctx, client, "echo hy2-ok")
	if err != nil || strings.Contains(out, "hy2-ok") {
		return ""
	}
	if m := forcedUserPattern.FindStringSubmatch(out); m != nil && m[1] != "root" {
		return m[1]
	}
	return ""
}

// switchSSHUser 把主机的 SSH 用户改成镜像要求的用户。主机密钥不变，指纹保留。
func (s *Server) switchSSHUser(ctx context.Context, serverID, user string) error {
	_, err := s.db.Exec(ctx, `UPDATE servers SET ssh_user=$1, updated_at=now() WHERE id=$2::uuid AND deleted_at IS NULL`, user, serverID)
	return err
}
