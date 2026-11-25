package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type currentOnlineSummary struct {
	IPs          []map[string]any
	Hy2Nodes     int
	RealityNodes int
	Expected     int
	Errors       []string
	SampledAt    *time.Time
}

func (s *Server) loadCurrentOnlineIPs(ctx context.Context, records []subscriptionRecord) (map[string]*currentOnlineSummary, error) {
	out := map[string]*currentOnlineSummary{}
	if len(records) == 0 {
		return out, nil
	}
	ids := []string{}
	expected := map[string]map[string]subscriptionNodeAccount{}
	for _, rec := range records {
		ids = append(ids, rec.ID)
		expected[rec.ID] = rec.nodeAccounts
		out[rec.ID] = &currentOnlineSummary{IPs: []map[string]any{}, Errors: []string{}, Expected: len(rec.nodeAccounts)}
	}
	rows, err := s.db.Query(ctx, `SELECT l.subscription_id::text,l.node_id::text,l.user_id,l.ips,l.hysteria_ok,l.reality_ok,l.error,l.reality_error,l.collected_at,n.name
 FROM subscription_live_connections l JOIN hysteria_nodes n ON n.id=l.node_id
 WHERE l.subscription_id=ANY($1::uuid[]) AND n.deleted_at IS NULL AND l.collected_at>now()-interval '90 seconds'`, ids)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var sub, node, user, name, hyErr, reErr string
		var ips []byte
		var hy, re bool
		var sampled time.Time
		if err := rows.Scan(&sub, &node, &user, &ips, &hy, &re, &hyErr, &reErr, &sampled, &name); err != nil {
			return out, err
		}
		acc, ok := expected[sub][node]
		if !ok || acc.UserID != user {
			continue
		}
		summary := out[sub]
		if hy {
			summary.Hy2Nodes++
		} else {
			summary.Errors = append(summary.Errors, name+"："+hyErr)
		}
		if re {
			summary.RealityNodes++
		} else {
			summary.Errors = append(summary.Errors, name+"："+reErr)
		}
		if summary.SampledAt == nil || sampled.Before(*summary.SampledAt) {
			t := sampled
			summary.SampledAt = &t
		}
		var entries []struct {
			IP       string `json:"ip"`
			Protocol string `json:"protocol"`
			LastSeen int64  `json:"last_seen"`
		}
		if err := json.Unmarshal(ips, &entries); err != nil {
			return out, fmt.Errorf("invalid live IP snapshot")
		}
		for _, ip := range entries {
			if ip.Protocol == "Hy2" && !hy || ip.Protocol == "Reality" && !re || (ip.Protocol != "Hy2" && ip.Protocol != "Reality") {
				continue
			}
			summary.IPs = append(summary.IPs, map[string]any{"ip": ip.IP, "protocol": ip.Protocol, "last_seen": ip.LastSeen, "node_id": node, "node_name": name})
		}
	}
	return out, rows.Err()
}
