package httpapi
import "testing"
func TestRealityOnlineActionValidation(t *testing.T) {
 spec,ok:=portalActions["enable_reality_online"]
 if !ok || !spec.Dangerous || spec.Fixed["target"]!="reality-online" {t.Fatal("missing confirmed fixed action")}
 if err:=validateActionForm(spec,map[string]string{"target":"reality-online"});err!=nil {t.Fatal(err)}
 if err:=validateActionForm(spec,map[string]string{"target":"arbitrary"});err==nil {t.Fatal("accepted unknown target")}
}
