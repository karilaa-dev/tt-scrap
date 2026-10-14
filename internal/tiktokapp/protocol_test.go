package tiktokapp

import (
	"net/url"
	"testing"
)

func TestRouteURLAppliesMatchingRulesInOrder(t *testing.T) {
	tc := func(hosts []string, replace string, selectors RoutingSelectors) RoutingRule {
		selectors.HostGroup, selectors.HostReplace = hosts, replace
		return RoutingRule{Action: "tc", Param: selectors}
	}
	cases := []struct {
		name  string
		input string
		rules []RoutingRule
		want  string
	}{
		{"no rules", "https://api19-va.tiktokv.com/aweme/v1/feed/?a=1", nil, "api19-va.tiktokv.com"},
		{"prefix", "https://api19-va.tiktokv.com/aweme/v1/feed/", []RoutingRule{tc([]string{"api19-va.tiktokv.com"}, "api22-normal-c-useast2a.tiktokv.com", RoutingSelectors{PrefixesGroup: []string{"/aweme/"}})}, "api22-normal-c-useast2a.tiktokv.com"},
		{"contains", "https://api19-va.tiktokv.com/aweme/v1/feed/", []RoutingRule{tc([]string{"*"}, "api16-va.tiktokv.com", RoutingSelectors{ContainGroup: []string{"/v1/"}})}, "api16-va.tiktokv.com"},
		{"equal", "https://api19-va.tiktokv.com/aweme/v1/feed/", []RoutingRule{tc([]string{"*.tiktokv.com"}, "api16-va.tiktokv.com", RoutingSelectors{EqualGroup: []string{"/aweme/v1/feed/"}})}, "api16-va.tiktokv.com"},
		{"equal mismatch", "https://api19-va.tiktokv.com/aweme/v1/feed/", []RoutingRule{tc([]string{"*.tiktokv.com"}, "api16-va.tiktokv.com", RoutingSelectors{EqualGroup: []string{"/aweme/v1/feed"}})}, "api19-va.tiktokv.com"},
		{"pattern", "https://api19-va.tiktokv.com/aweme/v2/feed/", []RoutingRule{tc([]string{"*.tiktokv.com"}, "api16-va.tiktokv.com", RoutingSelectors{PatternGroup: []string{`^/aweme/v\d+/feed/$`}})}, "api16-va.tiktokv.com"},
		{"pattern mismatch", "https://api19-va.tiktokv.com/aweme/v2/detail/", []RoutingRule{tc([]string{"*.tiktokv.com"}, "api16-va.tiktokv.com", RoutingSelectors{PatternGroup: []string{`^/aweme/v\d+/feed/$`}})}, "api19-va.tiktokv.com"},
		{"invalid pattern ignored", "https://api19-va.tiktokv.com/aweme/v1/feed/", []RoutingRule{tc([]string{"*"}, "api16-va.tiktokv.com", RoutingSelectors{PatternGroup: []string{"(", "/feed/"}})}, "api16-va.tiktokv.com"},
		{"only invalid pattern", "https://api19-va.tiktokv.com/aweme/v1/feed/", []RoutingRule{tc([]string{"*"}, "api16-va.tiktokv.com", RoutingSelectors{PatternGroup: []string{"("}})}, "api19-va.tiktokv.com"},
		{"host mismatch", "https://api19-va.tiktokv.com/aweme/v1/feed/", []RoutingRule{tc([]string{"log-va.tiktokv.com", "*.tiktokv.us"}, "api16-va.tiktokv.com", RoutingSelectors{PrefixesGroup: []string{"/"}})}, "api19-va.tiktokv.com"},
		{"untrusted target", "https://api19-va.tiktokv.com/aweme/v1/feed/", []RoutingRule{tc([]string{"*"}, "api.example.com", RoutingSelectors{PrefixesGroup: []string{"/"}})}, "api19-va.tiktokv.com"},
		{"dispatch", "https://api19-va.tiktokv.com/aweme/v1/feed/", []RoutingRule{{Action: "dispatch", Param: RoutingSelectors{HostGroup: []string{"*"}, PrefixesGroup: []string{"/aweme/"}, DispatchStrategy: 1, StrategyInfo: map[string]string{"api19-va.tiktokv.com": "api21-va.tiktokv.com"}}}}, "api21-va.tiktokv.com"},
		{"dispatch other strategy", "https://api19-va.tiktokv.com/aweme/v1/feed/", []RoutingRule{{Action: "dispatch", Param: RoutingSelectors{HostGroup: []string{"*"}, PrefixesGroup: []string{"/aweme/"}, DispatchStrategy: 2, StrategyInfo: map[string]string{"api19-va.tiktokv.com": "api21-va.tiktokv.com"}}}}, "api19-va.tiktokv.com"},
		{"chained", "https://api19-va.tiktokv.com/aweme/v1/feed/", []RoutingRule{
			tc([]string{"api19-va.tiktokv.com"}, "api16-va.tiktokv.com", RoutingSelectors{PrefixesGroup: []string{"/aweme/"}}),
			tc([]string{"api16-va.tiktokv.com"}, "api22-va.tiktokv.com", RoutingSelectors{PatternGroup: []string{"feed"}}),
		}, "api22-va.tiktokv.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original, err := url.Parse(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			routed := routeURL(original, tc.rules)
			if routed.Host != tc.want || routed.Path != original.Path || routed.RawQuery != original.RawQuery {
				t.Fatalf("routed to %s, want host %s", routed, tc.want)
			}
			if original.String() != tc.input {
				t.Fatalf("original URL changed to %s", original)
			}
		})
	}
}

func BenchmarkRouteURL(b *testing.B) {
	rules := []RoutingRule{
		{Action: "tc", Param: RoutingSelectors{HostGroup: []string{"*.tiktokv.com"}, PatternGroup: []string{`^/tiktok/v\d+/`, `^/aweme/v\d+/(?:feed|aweme/detail)/`}, HostReplace: "api16-normal-useast5.tiktokv.us"}},
		{Action: "tc", Param: RoutingSelectors{HostGroup: []string{"*.tiktokv.us"}, PatternGroup: []string{`^/passport/`, `^/aweme/v\d+/commit/`}, HostReplace: "api22-normal-c-useast2a.tiktokv.com"}},
		{Action: "dispatch", Param: RoutingSelectors{HostGroup: []string{"*"}, PatternGroup: []string{`^/service/\d+/`}, DispatchStrategy: 1, StrategyInfo: map[string]string{"log-va.tiktokv.com": "log16-normal-useast5.tiktokv.us"}}},
	}
	u, _ := url.Parse("https://api19-va.tiktokv.com/aweme/v1/aweme/detail/?aweme_id=7695962883523464478")
	b.ReportAllocs()
	for b.Loop() {
		if routeURL(u, rules).Host != "api16-normal-useast5.tiktokv.us" {
			b.Fatal("unexpected route")
		}
	}
}
