package ssrfguard

import "testing"

// TestCheckDialAddress 是 IP guard 表驱动：loopback/private/link-local/
// unspecified 拒、公网放行、allow_insecure 全放行、非 IP fail-closed
//（用例随实现自 packer/gitpack_test.go 迁入，断言口径不变）。
func TestCheckDialAddress(t *testing.T) {
	cases := []struct {
		address       string
		allowInsecure bool
		wantBlocked   bool
	}{
		{"127.0.0.1:443", false, true},
		{"127.8.8.8:443", false, true},                             // 127/8 整段
		{"10.1.2.3:80", false, true},                               // 10/8
		{"172.16.0.9:443", false, true},                            // 172.16/12
		{"192.168.1.1:443", false, true},                           // 192.168/16
		{"169.254.169.254:80", false, true},                        // link-local（云元数据端点）
		{"[::1]:443", false, true},                                 // IPv6 loopback
		{"[fe80::1]:443", false, true},                             // IPv6 link-local
		{"0.0.0.0:80", false, true},                                // unspecified
		{"93.184.216.34:443", false, false},                        // 公网样例
		{"[2606:2800:220:1:248:1893:25c8:1946]:443", false, false}, // 公网 IPv6
		{"127.0.0.1:443", true, false},                             // allow_insecure 全放行
		{"169.254.169.254:80", true, false},                        // allow_insecure 全放行
		{"example.invalid:443", false, true},                       // 非 IP fail-closed
	}
	for _, tc := range cases {
		err := CheckDialAddress(tc.address, tc.allowInsecure)
		if tc.wantBlocked && err == nil {
			t.Errorf("CheckDialAddress(%q, insecure=%v) = nil, want blocked", tc.address, tc.allowInsecure)
		}
		if !tc.wantBlocked && err != nil {
			t.Errorf("CheckDialAddress(%q, insecure=%v) = %v, want allowed", tc.address, tc.allowInsecure, err)
		}
	}
}
