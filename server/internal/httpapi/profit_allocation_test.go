package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProfitAllocatesAllRevenueToCurrentLiveNodes(t *testing.T) {
	for _, measured := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_samples", true: "old_node_samples"}[measured], func(t *testing.T) {
			db := fixDB(t)
			ctx := context.Background()
			_, err := db.Exec(ctx, `CREATE TABLE server_billing(server_id uuid,monthly_cost double precision,currency text);
CREATE TABLE subscription_measured_usage(subscription_id uuid,node_id uuid,measured_bytes bigint,
  quality_ok boolean DEFAULT true,collected_at timestamptz DEFAULT now());
INSERT INTO servers(id,name) VALUES ('00000000-0000-0000-0000-000000000103','fixture');
INSERT INTO hysteria_nodes(id,name,server_id) VALUES ('00000000-0000-0000-0000-000000000102','live','00000000-0000-0000-0000-000000000103');
INSERT INTO subscription_tokens(id,token,enabled,revenue_monthly,revenue_currency,node_accounts) VALUES
('00000000-0000-0000-0000-000000000101','profit-fixture',true,100,'CNY','{"00000000-0000-0000-0000-000000000102":{"user_id":"live"},"00000000-0000-0000-0000-000000000104":{"user_id":"deleted"}}');`)
			if err != nil {
				t.Fatal(err)
			}
			if measured {
				_, err = db.Exec(ctx, `INSERT INTO subscription_measured_usage VALUES
('00000000-0000-0000-0000-000000000101','00000000-0000-0000-0000-000000000102',10),
('00000000-0000-0000-0000-000000000101','00000000-0000-0000-0000-000000000104',90),
('00000000-0000-0000-0000-000000000101','00000000-0000-0000-0000-000000000105',900);`)
				if err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			fixServer(db).profitReport(w, httptest.NewRequest(http.MethodGet, "/api/reports/profit", nil))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var result struct {
				Nodes  []nodeProfit               `json:"nodes"`
				Totals map[string]moneyByCurrency `json:"totals"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Nodes) != 1 || result.Nodes[0].Revenue["CNY"] != 100 || result.Totals["revenue"]["CNY"] != 100 {
				t.Fatalf("lost revenue: %s", w.Body.String())
			}
		})
	}
}

// 2026-11-06：没有在役节点的主机成本、分不到节点的订阅收入都要进合计（记为未分摊），不能静默丢掉；
// 已删除主机的月费不计。
func TestProfitKeepsUnallocatedCostAndRevenue(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	_, err := db.Exec(ctx, `CREATE TABLE server_billing(server_id uuid,monthly_cost double precision,currency text);
CREATE TABLE subscription_measured_usage(subscription_id uuid,node_id uuid,measured_bytes bigint,
  quality_ok boolean DEFAULT true,collected_at timestamptz DEFAULT now());
INSERT INTO servers(id,name) VALUES ('00000000-0000-0000-0000-000000000103','empty host');
INSERT INTO servers(id,name,deleted_at) VALUES ('00000000-0000-0000-0000-000000000106','gone host',now());
INSERT INTO server_billing VALUES ('00000000-0000-0000-0000-000000000103',30,'CNY'),('00000000-0000-0000-0000-000000000106',99,'CNY');
INSERT INTO subscription_tokens(id,token,enabled,revenue_monthly,revenue_currency,node_accounts) VALUES
('00000000-0000-0000-0000-000000000101','profit-unallocated',true,50,'CNY','{}');`)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	fixServer(db).profitReport(w, httptest.NewRequest(http.MethodGet, "/api/reports/profit", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Totals             map[string]moneyByCurrency `json:"totals"`
		UnallocatedCost    moneyByCurrency            `json:"unallocated_cost"`
		UnallocatedRevenue moneyByCurrency            `json:"unallocated_revenue"`
		UnallocatedSubs    []string                   `json:"unallocated_subscription_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Totals["cost"]["CNY"] != 30 || result.UnallocatedCost["CNY"] != 30 {
		t.Fatalf("an empty host's cost must be counted (deleted host excluded): %s", w.Body.String())
	}
	if result.Totals["revenue"]["CNY"] != 50 || result.UnallocatedRevenue["CNY"] != 50 || len(result.UnallocatedSubs) != 1 {
		t.Fatalf("revenue without nodes must be counted as unallocated: %s", w.Body.String())
	}
	if result.Totals["profit"]["CNY"] != 20 {
		t.Fatalf("profit must include unallocated amounts, got %v", result.Totals["profit"])
	}
}
