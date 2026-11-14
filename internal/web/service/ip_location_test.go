package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestFormatIpLocation(t *testing.T) {
	tests := []struct {
		name string
		res  ipApiResponse
		want string
	}{
		{
			name: "Suzhou Unicom CNC Group AS4837",
			res: ipApiResponse{
				Status:      "success",
				Country:     "中国",
				CountryCode: "CN",
				RegionName:  "江苏",
				City:        "苏州市",
				ISP:         "CNC Group CHINA169 Jiangsu Province Network",
				AS:          "AS4837 CHINA UNICOM China169 Backbone",
			},
			want: "苏州联通",
		},
		{
			name: "Nanjing China Mobile AS56046",
			res: ipApiResponse{
				Status:      "success",
				Country:     "中国",
				CountryCode: "CN",
				RegionName:  "江苏",
				City:        "南京",
				ISP:         "China Mobile",
				AS:          "AS56046 China Mobile communications corporation",
			},
			want: "南京移动",
		},
		{
			name: "Shenzhen Chinanet AS4134",
			res: ipApiResponse{
				Status:      "success",
				Country:     "中国",
				CountryCode: "CN",
				RegionName:  "广东",
				City:        "深圳",
				ISP:         "Chinanet",
				AS:          "AS4134 CHINANET-BACKBONE",
			},
			want: "深圳电信",
		},
		{
			name: "Taipei Chunghwa Telecom",
			res: ipApiResponse{
				Status:      "success",
				Country:     "台湾",
				CountryCode: "TW",
				City:        "台北市",
				ISP:         "Chunghwa Telecom Co., Ltd.",
				AS:          "AS3462 Data Communication Business Group",
			},
			want: "台北中华电信",
		},
		{
			name: "Hong Kong IXTech",
			res: ipApiResponse{
				Status:      "success",
				Country:     "中国香港",
				CountryCode: "HK",
				City:        "下葵涌",
				ISP:         "IXTech Limited",
				AS:          "AS132839 IXTech Limited",
			},
			want: "香港 IXTech",
		},
		{
			name: "Macau CTM",
			res: ipApiResponse{
				Status:      "success",
				Country:     "中国澳门",
				CountryCode: "MO",
				City:        "Macau",
				ISP:         "CTM",
			},
			want: "澳门 CTM",
		},
		{
			name: "Singapore AWS",
			res: ipApiResponse{
				Status:      "success",
				Country:     "新加坡",
				CountryCode: "SG",
				City:        "新加坡",
				ISP:         "Amazon Data Services Singapore",
				AS:          "AS16509 Amazon.com, Inc.",
			},
			want: "新加坡 AWS",
		},
		{
			name: "Japan Tokyo Oracle",
			res: ipApiResponse{
				Status:      "success",
				Country:     "日本",
				CountryCode: "JP",
				RegionName:  "东京都",
				City:        "Tokyo",
				ISP:         "Oracle Corporation",
				AS:          "AS31898 Oracle Corporation",
			},
			want: "东京 Oracle",
		},
		{
			name: "US Ashburn Google",
			res: ipApiResponse{
				Status:      "success",
				Country:     "美国",
				CountryCode: "US",
				City:        "Ashburn",
				ISP:         "Google LLC",
				AS:          "AS15169 Google LLC",
			},
			want: "Ashburn Google",
		},
		{
			name: "US T-Mobile must not falsely match China Mobile",
			res: ipApiResponse{
				Status:      "success",
				Country:     "美国",
				CountryCode: "US",
				City:        "Seattle",
				ISP:         "T-Mobile USA, Inc.",
				AS:          "AS21928 T-Mobile USA, Inc.",
			},
			want: "Seattle T-Mobile USA",
		},
		{
			name: "Fail status returns empty string",
			res: ipApiResponse{
				Status: "fail",
				City:   "Beijing",
				ISP:    "Test",
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatIpLocation(tc.res); got != tc.want {
				t.Fatalf("FormatIpLocation() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeQueryIP(t *testing.T) {
	tests := []struct {
		raw       string
		wantCanon string
		wantPub   bool
		wantLabel string
	}{
		{"127.0.0.1", "127.0.0.1", false, "本地"},
		{"::1", "::1", false, "本地"},
		{"[::1]", "::1", false, "本地"},
		{"10.0.0.1", "10.0.0.1", false, "内网"},
		{"192.168.1.5", "192.168.1.5", false, "内网"},
		{"100.64.0.1", "100.64.0.1", false, "内网"},
		{"[fe80::1%eth0]", "fe80::1", false, "内网"},
		{"::ffff:221.6.10.213", "221.6.10.213", true, ""},
		{"[::ffff:221.6.10.213]", "221.6.10.213", true, ""},
		{"221.6.10.213:12345", "221.6.10.213", true, ""},
		{"2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d", "2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d", true, ""},
		{"[2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d]", "2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d", true, ""},
		{"[2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d]:54321", "2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d", true, ""},
		{"not-an-ip", "", false, ""},
		{"", "", false, ""},
	}

	for _, tc := range tests {
		gotCanon, gotPub, gotLabel := normalizeQueryIP(tc.raw)
		if gotCanon != tc.wantCanon || gotPub != tc.wantPub || gotLabel != tc.wantLabel {
			t.Errorf(
				"normalizeQueryIP(%q) = (%q, %v, %q), want (%q, %v, %q)",
				tc.raw, gotCanon, gotPub, gotLabel, tc.wantCanon, tc.wantPub, tc.wantLabel,
			)
		}
	}
}

func TestIpLocationService_LookupLocations_SingleBatchCacheAnd429(t *testing.T) {
	var singleCalls atomic.Int32
	var batchCalls atomic.Int32
	var force429 atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if force429.Load() {
			w.Header().Set("X-Ttl", "15")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/json/221.6.10.213":
			singleCalls.Add(1)
			_ = json.NewEncoder(w).Encode(ipApiResponse{
				Status:      "success",
				Country:     "中国",
				CountryCode: "CN",
				City:        "苏州市",
				ISP:         "CNC Group CHINA169 Jiangsu Province Network",
				AS:          "AS4837 CHINA UNICOM China169 Backbone",
				Query:       "221.6.10.213",
			})
		case "/json/2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d":
			singleCalls.Add(1)
			_ = json.NewEncoder(w).Encode(ipApiResponse{
				Status:      "success",
				Country:     "中国",
				CountryCode: "CN",
				RegionName:  "广东",
				City:        "深圳",
				ISP:         "China Mobile Communications Group Co., Ltd",
				Org:         "China Mobile Communications Group Co., Ltd",
				AS:          "AS56040 China Mobile communications corporation",
				Query:       "2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d",
			})
		case "/batch":
			batchCalls.Add(1)
			var reqIPs []string
			if err := json.NewDecoder(r.Body).Decode(&reqIPs); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			resp := make([]ipApiResponse, 0, len(reqIPs))
			for _, ip := range reqIPs {
				switch ip {
				case "112.25.223.109":
					resp = append(resp, ipApiResponse{
						Status:      "success",
						CountryCode: "CN",
						City:        "南京",
						AS:          "AS56046 China Mobile communications corporation",
						Query:       ip,
					})
				case "14.154.206.197":
					resp = append(resp, ipApiResponse{
						Status:      "success",
						CountryCode: "CN",
						City:        "深圳",
						ISP:         "Chinanet",
						Query:       ip,
					})
				default:
					resp = append(resp, ipApiResponse{
						Status:      "success",
						CountryCode: "SG",
						City:        "新加坡",
						AS:          "AS16509 Amazon.com, Inc.",
						Query:       ip,
					})
				}
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	svc := newIpLocationService()
	svc.apiBaseURL = srv.URL
	svc.httpClient = srv.Client()
	svc.nowFn = func() time.Time { return now }

	// Single IP hits GET /json/:ip; local IPs do not trigger HTTP calls.
	got := svc.LookupLocations([]string{"221.6.10.213", "127.0.0.1", "100.64.0.1"})
	if got["221.6.10.213"] != "苏州联通" || got["127.0.0.1"] != "本地" || got["100.64.0.1"] != "内网" {
		t.Fatalf("unexpected single lookup result: %v", got)
	}
	if singleCalls.Load() != 1 || batchCalls.Load() != 0 {
		t.Fatalf("expected 1 single and 0 batch calls, got single=%d batch=%d", singleCalls.Load(), batchCalls.Load())
	}

	// Cache hit skips HTTP.
	cachedOnly := svc.GetCachedLocations([]string{"221.6.10.213", "192.168.1.1"})
	if cachedOnly["221.6.10.213"] != "苏州联通" || cachedOnly["192.168.1.1"] != "内网" {
		t.Fatalf("unexpected GetCachedLocations result: %v", cachedOnly)
	}
	gotCached := svc.LookupLocations([]string{"221.6.10.213"})
	if gotCached["221.6.10.213"] != "苏州联通" || singleCalls.Load() != 1 {
		t.Fatalf("expected cache hit without HTTP call, singleCalls=%d got=%v", singleCalls.Load(), gotCached)
	}

	// Multiple missing IPs hit POST /batch.
	gotBatch := svc.LookupLocations([]string{"112.25.223.109", "14.154.206.197", "221.6.10.213"})
	if gotBatch["112.25.223.109"] != "南京移动" || gotBatch["14.154.206.197"] != "深圳电信" || gotBatch["221.6.10.213"] != "苏州联通" {
		t.Fatalf("unexpected batch result: %v", gotBatch)
	}
	if batchCalls.Load() != 1 || singleCalls.Load() != 1 {
		t.Fatalf("expected 1 batch and 1 single call, got single=%d batch=%d", singleCalls.Load(), batchCalls.Load())
	}

	// 429 enters cooldown based on X-Ttl and skips further HTTP calls until cooldown expires.
	force429.Store(true)
	gotLimited := svc.LookupLocations([]string{"52.74.0.1"})
	if _, ok := gotLimited["52.74.0.1"]; ok {
		t.Fatalf("expected empty result for rate-limited IP, got %v", gotLimited)
	}
	if singleCalls.Load() != 1 {
		t.Fatalf("429 handler should not have incremented singleCalls, got %d", singleCalls.Load())
	}

	force429.Store(false)
	// Still inside 15s cooldown: cached IPs work, new uncached IP does not trigger HTTP.
	svc.LookupLocations([]string{"52.74.0.1"})
	if singleCalls.Load() != 1 {
		t.Fatalf("expected cooldown to skip HTTP calls")
	}

	now = now.Add(16 * time.Second)
	svc.apiBaseURL = srv.URL
	// Advance past cooldown: next batch lookup hits /batch default branch -> 新加坡 AWS.
	gotAfter := svc.LookupLocations([]string{"52.74.0.1", "8.8.8.8"})
	if gotAfter["52.74.0.1"] != "新加坡 AWS" {
		t.Fatalf("expected lookup after cooldown to succeed, got %v", gotAfter)
	}
	if batchCalls.Load() != 2 {
		t.Fatalf("expected batchCalls=2 after cooldown recovery, got %d", batchCalls.Load())
	}

	// Bracketed IPv6 from Xray logs queries unbracketed canonical IPv6 and maps back to bracketed key.
	gotIPv6 := svc.LookupLocations([]string{"[2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d]"})
	if gotIPv6["[2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d]"] != "深圳移动" {
		t.Fatalf("expected bracketed IPv6 lookup to return 深圳移动, got %v", gotIPv6)
	}
	if singleCalls.Load() != 2 {
		t.Fatalf("expected singleCalls=2 after bracketed IPv6 lookup, got %d", singleCalls.Load())
	}
	cachedIPv6 := svc.GetCachedLocations([]string{"[2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d]", "2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d"})
	if cachedIPv6["[2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d]"] != "深圳移动" || cachedIPv6["2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d"] != "深圳移动" {
		t.Fatalf("expected cached bracketed/unbracketed IPv6 both to return 深圳移动, got %v", cachedIPv6)
	}
}
