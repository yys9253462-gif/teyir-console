package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTrafficResetDayPreservesOnlyCurrentCycleUsage(t *testing.T) {
	for _, current := range []bool{true, false} {
		t.Run(map[bool]string{true: "current", false: "stale"}[current], func(t *testing.T) {
			s := repairSubscriptionDB(t)
			ctx := context.Background()
			_, err := s.db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD traffic_limit_bytes bigint, ADD traffic_reset_day int DEFAULT 1, ADD traffic_cycle_start timestamptz, ADD traffic_cycle_bytes bigint DEFAULT 0`)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			oldDay := now.In(trafficCycleZone).Day()
			newDay := oldDay%31 + 1
			start := trafficCycleStart(now, oldDay)
			if !current {
				start = start.AddDate(0, -2, 0)
			}
			_, err = s.db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,traffic_reset_day,traffic_cycle_start,traffic_cycle_bytes) VALUES($1,'fixture',$2,$3,12345)`, repairFirst, oldDay, start)
			if err != nil {
				t.Fatal(err)
			}
			body := `{"limit_gb":100,"reset_day":` + strconv.Itoa(newDay) + `}`
			req := httptest.NewRequest(http.MethodPut, "/api/nodes/fixture/traffic-limit", strings.NewReader(body))
			req.SetPathValue("id", repairFirst)
			w := httptest.NewRecorder()
			s.updateNodeTrafficLimit(w, req)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var got time.Time
			var used int64
			if err = s.db.QueryRow(ctx, `SELECT traffic_cycle_start,traffic_cycle_bytes FROM hysteria_nodes WHERE id=$1`, repairFirst).Scan(&got, &used); err != nil {
				t.Fatal(err)
			}
			want := start
			if current {
				want = trafficCycleStart(now, newDay)
			}
			if !got.Equal(want) || used != 12345 {
				t.Fatalf("start=%v want=%v bytes=%d", got, want, used)
			}
		})
	}
}
