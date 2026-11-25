package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// china_quality.go —— 三网质量趋势与持续丢包告警（2026-11-01）。
//
// 数据早就有：node_speedtests 每个节点约 15 分钟一轮、保留 30 天。
// 缺的是两件事：
//   1. 只展示「最近一轮」，看不出「从哪天开始变差」「晚高峰丢多少」；
//   2. 线路劣化没有告警，只能等客户说连不上。
//
// 口径：只算 telecom / unicom / mobile 三家（other 是公共 DNS，不代表回国线路）。
// 单靶点偶发 100% 丢包很常见（实测每靶点每天约 5% 的轮次），所以一律按
// 「运营商 × 时间桶」取平均，不对单点单轮做判断。

// 区间 → 桶宽。点数控制在 ~100–200，前端画线不卡、也看得清晚高峰。
var chinaQualityRanges = map[string]struct {
	window, bucket time.Duration
}{
	"24h": {24 * time.Hour, 15 * time.Minute},
	"7d":  {7 * 24 * time.Hour, time.Hour},
	"30d": {30 * 24 * time.Hour, 4 * time.Hour},
}

type chinaQualityPoint struct {
	At      time.Time `json:"t"`
	AvgMs   *float64  `json:"avg_ms"`
	LossPct *float64  `json:"loss_pct"`
}

type chinaQualitySeries struct {
	Carrier string              `json:"carrier"`
	Label   string              `json:"label"`
	AvgMs   *float64            `json:"avg_ms"`
	P95Ms   *float64            `json:"p95_ms"`
	LossPct *float64            `json:"loss_pct"`
	Points  []chinaQualityPoint `json:"points"`
}

func (s *Server) nodeChinaQuality(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	nodeID := r.PathValue("id")
	if !uuidPattern.MatchString(nodeID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "节点 ID 无效"})
		return
	}
	rangeKey := r.URL.Query().Get("range")
	if rangeKey == "" {
		rangeKey = "24h"
	}
	spec, ok := chinaQualityRanges[rangeKey]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "range 只支持 24h / 7d / 30d"})
		return
	}
	var exists bool
	if err := s.db.QueryRow(r.Context(), `SELECT true FROM hysteria_nodes WHERE id=$1::uuid AND deleted_at IS NULL`, nodeID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	series, err := s.loadChinaQuality(r.Context(), nodeID, spec.window, spec.bucket)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range":          rangeKey,
		"bucket_seconds": int(spec.bucket.Seconds()),
		"series":         series,
	})
}

func (s *Server) loadChinaQuality(ctx context.Context, nodeID string, window, bucket time.Duration) ([]chinaQualitySeries, error) {
	byCarrier := map[string]*chinaQualitySeries{}
	for _, carrier := range chinaQualityCarriers {
		byCarrier[carrier] = &chinaQualitySeries{Carrier: carrier, Label: carrierLabel(carrier), Points: []chinaQualityPoint{}}
	}

	rows, err := s.db.Query(ctx, `
SELECT carrier,
       date_bin(make_interval(secs => $3), measured_at, TIMESTAMPTZ '2000-01-01') AS bucket,
       avg(icmp_avg_ms), avg(icmp_loss_pct)
FROM node_speedtests
WHERE node_id = $1::uuid AND measured_at > now() - make_interval(secs => $2)
  AND carrier IN ('telecom','unicom','mobile') AND probe_ok
GROUP BY 1, 2 ORDER BY 2`, nodeID, window.Seconds(), bucket.Seconds())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var carrier string
		var p chinaQualityPoint
		if err := rows.Scan(&carrier, &p.At, &p.AvgMs, &p.LossPct); err != nil {
			rows.Close()
			return nil, err
		}
		if series := byCarrier[carrier]; series != nil {
			series.Points = append(series.Points, p)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// 区间汇总单独算：桶平均的平均会把少样本的桶放大，P95 也只能在原始行上取。
	rows, err = s.db.Query(ctx, `
SELECT carrier, avg(icmp_avg_ms),
       percentile_cont(0.95) WITHIN GROUP (ORDER BY icmp_avg_ms) FILTER (WHERE icmp_avg_ms IS NOT NULL),
       avg(icmp_loss_pct)
FROM node_speedtests
WHERE node_id = $1::uuid AND measured_at > now() - make_interval(secs => $2)
  AND carrier IN ('telecom','unicom','mobile') AND probe_ok
GROUP BY 1`, nodeID, window.Seconds())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var carrier string
		var avg, p95, loss *float64
		if err := rows.Scan(&carrier, &avg, &p95, &loss); err != nil {
			rows.Close()
			return nil, err
		}
		if series := byCarrier[carrier]; series != nil {
			series.AvgMs, series.P95Ms, series.LossPct = avg, p95, loss
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]chinaQualitySeries, 0, len(chinaQualityCarriers))
	for _, carrier := range chinaQualityCarriers {
		out = append(out, *byCarrier[carrier])
	}
	return out, nil
}

var chinaQualityCarriers = []string{"telecom", "unicom", "mobile"}

// 持续丢包告警阈值。基线实测：三网单轮平均丢包 5%–14%（含偶发全丢的靶点），
// 所以触发线放在 30%，并要求近 1 小时内至少 2 轮；恢复线 15% 做滞回，
// 避免在阈值附近每 15 分钟开关一次。
const (
	chinaLossTriggerPct = 30.0
	chinaLossRecoverPct = 15.0
	chinaLossMinRounds  = 2
)

// chinaLossConditions 产出 node_china_loss 告警条件。
//
// 排除：非 online 节点（离线已有 node_offline）、已被判定被墙的节点
// （node_blocked 已是 critical，同一故障不重复推送）。
// 已 open 的告警按恢复线判定是否仍成立，这就是滞回。
func (s *Server) chinaLossConditions(ctx context.Context) ([]alertCondition, error) {
	rows, err := s.db.Query(ctx, chinaLossConditionsSQL, chinaLossTriggerPct, chinaLossRecoverPct, chinaLossMinRounds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alertCondition
	for rows.Next() {
		var sid, nid string
		if err := rows.Scan(&sid, &nid); err != nil {
			return nil, err
		}
		out = append(out, alertCondition{alertKey{"node_china_loss", sid, nid}, "warning"})
	}
	return out, rows.Err()
}

const chinaLossConditionsSQL = `
WITH recent AS (
    SELECT st.node_id, st.carrier, avg(st.icmp_loss_pct) AS loss, count(DISTINCT st.round_id) AS rounds
    FROM node_speedtests st
    WHERE st.measured_at > now() - interval '1 hour'
      AND st.carrier IN ('telecom','unicom','mobile') AND st.probe_ok
    GROUP BY 1, 2
)
SELECT DISTINCT n.server_id::text, n.id::text
FROM hysteria_nodes n
JOIN recent r ON r.node_id = n.id
WHERE n.deleted_at IS NULL AND n.status = 'online' AND r.rounds >= $3
  AND NOT EXISTS (SELECT 1 FROM alerts a WHERE a.node_id = n.id AND a.type = 'node_blocked' AND a.status <> 'resolved')
  AND (r.loss >= $1
       OR (r.loss >= $2 AND EXISTS (SELECT 1 FROM alerts a WHERE a.node_id = n.id AND a.type = 'node_china_loss' AND a.status <> 'resolved')))`
