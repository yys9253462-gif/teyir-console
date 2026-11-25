package ippurity

import (
	"encoding/json"
	"testing"
)

// 用实测的 RDAP 响应形状（2026-10-15）做回归。
func TestCountryFromRDAP(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		// APNIC：网段对象直接带 country（阿里云韩国段登记在新加坡）。
		{"apnic-country-field", `{"country":"SG","port43":"whois.apnic.net"}`, "SG"},
		// ARIN：没有 country，取 registrant 的 vCard 地址最后一行。
		{"arin-registrant-label", `{"port43":"whois.arin.net","entities":[
			{"roles":["abuse"],"vcardArray":["vcard",[["adr",{"label":"410 Terry Ave N.\nSeattle\nWA\n98109\nUnited States"},"text",["","","","","","",""]]]]},
			{"roles":["registrant"],"vcardArray":["vcard",[["adr",{"label":"Kungsgatan 49\nStockholm\n111 22\nSweden"},"text",["","","","","","",""]]]]}
		]}`, "SE"},
		// abuse 联系人在美国，但不能拿它当注册国。
		{"arin-ignores-abuse-only", `{"entities":[{"roles":["abuse"],"vcardArray":["vcard",[["adr",{"label":"Seattle\nUnited States"},"text",["","","","","","",""]]]]}]}`, ""},
	}
	for _, c := range cases {
		var n rdapNetwork
		if err := json.Unmarshal([]byte(c.json), &n); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, ok := countryFromRDAP(n)
		if c.want == "" {
			if ok {
				t.Errorf("%s: 应为未判定，实际 %q", c.name, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("%s: 想要 %s，实际 %q (ok=%v)", c.name, c.want, got, ok)
		}
	}
}
