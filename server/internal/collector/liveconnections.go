package collector

import (
	"context"
	"encoding/json"
)

type liveUserSnapshot struct {
	IPs          json.RawMessage `json:"ips"`
	OK           bool            `json:"ok"`
	Error        string          `json:"error"`
	HysteriaOK   bool            `json:"hysteria_ok"`
	RealityOK    bool            `json:"reality_ok"`
	RealityError string          `json:"reality_error"`
}

func (c *Collector) replaceLiveConnection(ctx context.Context, sub, node, user string, snapshot *liveUserSnapshot) {
	u := liveUserSnapshot{Error: "节点门户尚未提供当前在线IP数据", RealityError: "Reality当前在线IP未知"}
	if snapshot != nil {
		u = *snapshot
	}
	if len(u.IPs) == 0 {
		u.IPs = json.RawMessage(`[]`)
	}
	_, err := c.db.Exec(ctx, `INSERT INTO subscription_live_connections
 (subscription_id,node_id,user_id,ips,ok,error,hysteria_ok,reality_ok,reality_error,collected_at)
 VALUES($1::uuid,$2::uuid,$3,$4::jsonb,$5,$6,$7,$8,$9,now())
 ON CONFLICT(subscription_id,node_id) DO UPDATE SET
 user_id=EXCLUDED.user_id,ips=EXCLUDED.ips,ok=EXCLUDED.ok,error=EXCLUDED.error,
 hysteria_ok=EXCLUDED.hysteria_ok,reality_ok=EXCLUDED.reality_ok,reality_error=EXCLUDED.reality_error,collected_at=now()`,
		sub, node, user, string(u.IPs), u.OK, u.Error, u.HysteriaOK, u.RealityOK, u.RealityError)
	if err != nil {
		c.logger.Warn("collector: live IP write failed", "error", err)
	}
}
