package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mhsanaei/3x-ui/v3/internal/logger"
	"golang.org/x/sync/singleflight"
)

const (
	ipApiDefaultBaseURL         = "http://ip-api.com"
	bilibiliDefaultBaseURL      = "https://api.live.bilibili.com"
	ipinfoDefaultBaseURL        = "https://ipinfo.io"
	ipApiFieldsParam            = "status,country,countryCode,regionName,city,isp,org,as,query"
	ipLocationCacheTTL          = 24 * time.Hour
	ipLocationMaxCacheEntries   = 4096
	ipLocationRequestTimeout    = 4 * time.Second
	ipLocationCalibrateTimeout  = 5 * time.Second
	ipLocationCalibrateSubLimit = 2500 * time.Millisecond
	ipLocationCalibrateWorkers  = 5
	ipLocationCalibrateLimit    = 40
	ipLocationFallbackLimit     = 20
	ipLocationBatchLimit        = 100
	ipLocationRateLimitDefault  = 30 * time.Second
	bilibiliRateLimitDefault    = 60 * time.Second
	ipLocationMaxInputIPs       = 500
	ipApiMaxResponseBytes       = 512 << 10
)

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

type ipApiResponse struct {
	Status      string `json:"status"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	RegionName  string `json:"regionName"`
	City        string `json:"city"`
	ISP         string `json:"isp"`
	Org         string `json:"org"`
	AS          string `json:"as"`
	Query       string `json:"query"`
}

type bilibiliIpResponse struct {
	Code int            `json:"code"`
	Data bilibiliIpData `json:"data"`
}

type bilibiliIpData struct {
	Addr     string `json:"addr"`
	Country  string `json:"country"`
	Province string `json:"province"`
	City     string `json:"city"`
	ISP      string `json:"isp"`
}

type ipInfoResponse struct {
	IP      string `json:"ip"`
	City    string `json:"city"`
	Region  string `json:"region"`
	Country string `json:"country"`
	Org     string `json:"org"`
}

type ipLocationCacheEntry struct {
	location  string
	expiresAt time.Time
}

// IpLocationService resolves public client IPs into concise "city + ISP" labels.
// It uses ip-api.com as the primary/overseas batch source and calibrates Chinese
// mainland IPs via Bilibili and ipinfo.io with bounded in-memory caching.
type IpLocationService struct {
	mu                  sync.RWMutex
	cache               map[string]ipLocationCacheEntry
	order               []string
	group               singleflight.Group
	cooldownUntil       time.Time
	bilibiliCooldown    time.Time
	ipinfoCooldown      time.Time
	httpClient          *http.Client
	apiBaseURL          string
	bilibiliBaseURL     string
	ipinfoBaseURL       string
	nowFn               func() time.Time
}

var defaultIpLocationService = newIpLocationService()

// DefaultIpLocationService returns the shared IP geolocation service instance.
func DefaultIpLocationService() *IpLocationService {
	return defaultIpLocationService
}

func newIpLocationService() *IpLocationService {
	return &IpLocationService{
		cache:      make(map[string]ipLocationCacheEntry),
		apiBaseURL: ipApiDefaultBaseURL,
		nowFn:      time.Now,
	}
}

func (s *IpLocationService) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

func (s *IpLocationService) client() *http.Client {
	if s.httpClient != nil {
		return s.httpClient
	}
	return (&SettingService{}).NewProxiedHTTPClient(ipLocationRequestTimeout)
}

func (s *IpLocationService) baseURL() string {
	if s.apiBaseURL != "" {
		return strings.TrimRight(s.apiBaseURL, "/")
	}
	return ipApiDefaultBaseURL
}

func (s *IpLocationService) isCustomApiURL() bool {
	return s.apiBaseURL != "" && s.apiBaseURL != ipApiDefaultBaseURL
}

func (s *IpLocationService) bilibiliURL() string {
	if s.bilibiliBaseURL != "" {
		if s.bilibiliBaseURL == "-" {
			return ""
		}
		return strings.TrimRight(s.bilibiliBaseURL, "/")
	}
	if s.isCustomApiURL() {
		return ""
	}
	return bilibiliDefaultBaseURL
}

func (s *IpLocationService) ipinfoURL() string {
	if s.ipinfoBaseURL != "" {
		if s.ipinfoBaseURL == "-" {
			return ""
		}
		return strings.TrimRight(s.ipinfoBaseURL, "/")
	}
	if s.isCustomApiURL() {
		return ""
	}
	return ipinfoDefaultBaseURL
}

func (s *IpLocationService) hasActiveSource(now time.Time) bool {
	if !now.Before(s.cooldownUntil) {
		return true
	}
	if s.ipinfoURL() != "" && !now.Before(s.ipinfoCooldown) {
		return true
	}
	return s.bilibiliURL() != "" && !now.Before(s.bilibiliCooldown)
}

// GetCachedLocations returns cached or local labels for ips without making
// any external HTTP calls.
func (s *IpLocationService) GetCachedLocations(ips []string) map[string]string {
	out := make(map[string]string)
	if len(ips) == 0 {
		return out
	}

	now := s.now()
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, raw := range ips {
		canonical, isPublic, privateLabel := normalizeQueryIP(raw)
		if canonical == "" {
			continue
		}
		if !isPublic {
			out[raw] = privateLabel
			continue
		}
		if entry, ok := s.cache[canonical]; ok && now.Before(entry.expiresAt) {
			out[raw] = entry.location
		}
	}
	return out
}

// LookupLocations resolves locations for the given IPs, serving local and
// cached entries immediately and querying ip-api.com for missing public IPs.
func (s *IpLocationService) LookupLocations(ips []string) map[string]string {
	out := make(map[string]string)
	if len(ips) == 0 {
		return out
	}

	type mapping struct {
		raw       string
		canonical string
	}
	mappings := make([]mapping, 0, min(len(ips), ipLocationMaxInputIPs))
	missingSet := make(map[string]struct{})
	missing := make([]string, 0)

	now := s.now()
	s.mu.RLock()
	for _, raw := range ips {
		if len(mappings) >= ipLocationMaxInputIPs {
			break
		}
		canonical, isPublic, privateLabel := normalizeQueryIP(raw)
		if canonical == "" {
			continue
		}
		if !isPublic {
			out[raw] = privateLabel
			continue
		}
		mappings = append(mappings, mapping{raw: raw, canonical: canonical})
		if entry, ok := s.cache[canonical]; ok && now.Before(entry.expiresAt) {
			out[raw] = entry.location
			continue
		}
		if _, seen := missingSet[canonical]; !seen {
			missingSet[canonical] = struct{}{}
			missing = append(missing, canonical)
		}
	}
	inCooldown := !s.hasActiveSource(now)
	s.mu.RUnlock()

	if len(missing) == 0 || inCooldown {
		return out
	}

	slices.Sort(missing)
	sfKey := strings.Join(missing, ",")
	_, _, _ = s.group.Do(sfKey, func() (any, error) {
		toFetch := s.filterUncached(missing)
		if len(toFetch) > 0 {
			s.fetchAndCache(toFetch)
		}
		return nil, nil
	})

	now = s.now()
	s.mu.RLock()
	for _, m := range mappings {
		if _, ok := out[m.raw]; ok {
			continue
		}
		if entry, ok := s.cache[m.canonical]; ok && now.Before(entry.expiresAt) {
			out[m.raw] = entry.location
		}
	}
	s.mu.RUnlock()

	return out
}

func (s *IpLocationService) filterUncached(canonicals []string) []string {
	now := s.now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.hasActiveSource(now) {
		return nil
	}
	out := make([]string, 0, len(canonicals))
	for _, ip := range canonicals {
		if entry, ok := s.cache[ip]; ok && now.Before(entry.expiresAt) {
			continue
		}
		out = append(out, ip)
	}
	return out
}

func (s *IpLocationService) fetchAndCache(canonicals []string) {
	primary := s.fetchPrimaryIpApi(canonicals)
	resolved := make(map[string]string, len(canonicals))

	var cnTargets []string
	var fallbackTargets []string
	for _, ip := range canonicals {
		if res, ok := primary[ip]; ok {
			if loc := FormatIpLocation(res); loc != "" {
				resolved[ip] = loc
			}
			if strings.EqualFold(strings.TrimSpace(res.CountryCode), "CN") {
				cnTargets = append(cnTargets, ip)
			} else if resolved[ip] == "" {
				fallbackTargets = append(fallbackTargets, ip)
			}
			continue
		}
		fallbackTargets = append(fallbackTargets, ip)
	}

	s.calibrateAndFallback(resolved, primary, cnTargets, fallbackTargets)
	if len(resolved) > 0 {
		s.putCache(resolved)
	}
}

func (s *IpLocationService) fetchPrimaryIpApi(canonicals []string) map[string]ipApiResponse {
	s.mu.RLock()
	inCooldown := s.now().Before(s.cooldownUntil)
	s.mu.RUnlock()
	if inCooldown || len(canonicals) == 0 {
		return nil
	}

	raw := make(map[string]ipApiResponse, len(canonicals))
	if len(canonicals) == 1 {
		res, rateLimited, err := s.fetchSingle(canonicals[0])
		if rateLimited {
			return nil
		}
		if err != nil {
			logger.Warningf("ip-api single lookup failed for %s: %v", canonicals[0], err)
			return nil
		}
		if res.Status == "" || res.Status == "success" {
			raw[canonicals[0]] = res
		}
		return raw
	}

	for start := 0; start < len(canonicals); start += ipLocationBatchLimit {
		end := min(start+ipLocationBatchLimit, len(canonicals))
		chunk := canonicals[start:end]
		items, rateLimited, err := s.fetchBatchRaw(chunk)
		for ip, res := range items {
			raw[ip] = res
		}
		if rateLimited {
			break
		}
		if err != nil {
			logger.Warningf("ip-api batch lookup failed (%d IPs): %v", len(chunk), err)
		}
	}
	return raw
}

func (s *IpLocationService) calibrateAndFallback(
	resolved map[string]string,
	primary map[string]ipApiResponse,
	cnTargets []string,
	fallbackTargets []string,
) {
	if s.bilibiliURL() == "" && s.ipinfoURL() == "" {
		return
	}

	type calibrateTask struct {
		ip   string
		isCN bool
	}
	maxCN := min(len(cnTargets), ipLocationCalibrateLimit)
	maxFallback := min(len(fallbackTargets), ipLocationFallbackLimit)
	tasks := make([]calibrateTask, 0, maxCN+maxFallback)
	for _, ip := range cnTargets[:maxCN] {
		tasks = append(tasks, calibrateTask{ip: ip, isCN: true})
	}
	for _, ip := range fallbackTargets[:maxFallback] {
		tasks = append(tasks, calibrateTask{ip: ip, isCN: false})
	}
	if len(tasks) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), ipLocationCalibrateTimeout)
	defer cancel()

	results := make([]string, len(tasks))
	jobs := make(chan int, len(tasks))
	for i := range tasks {
		jobs <- i
	}
	close(jobs)

	workers := min(ipLocationCalibrateWorkers, len(tasks))
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				t := tasks[idx]
				var primaryPtr *ipApiResponse
				if res, ok := primary[t.ip]; ok {
					primaryPtr = &res
				}
				results[idx] = s.resolveCalibrated(ctx, t.ip, primaryPtr, t.isCN)
			}
		}()
	}
	wg.Wait()

	for i, t := range tasks {
		if results[i] != "" {
			resolved[t.ip] = results[i]
		}
	}
}

func (s *IpLocationService) resolveCalibrated(ctx context.Context, ip string, primary *ipApiResponse, isCN bool) string {
	if isCN {
		if loc := s.lookupBilibiliLocation(ctx, ip, primary); loc != "" {
			return loc
		}
		if info, err := s.fetchIpInfo(ctx, ip); err == nil {
			if shouldUseIpInfoFallback(primary, info) {
				if loc := FormatIpInfoLocation(info); loc != "" {
					return loc
				}
			}
		}
		return ""
	}

	if info, err := s.fetchIpInfo(ctx, ip); err == nil {
		if strings.EqualFold(strings.TrimSpace(info.Country), "CN") {
			if loc := s.lookupBilibiliLocation(ctx, ip, primary); loc != "" {
				return loc
			}
		}
		if loc := FormatIpInfoLocation(info); loc != "" {
			return loc
		}
	}

	if looksLikeCNIP(ip) {
		return s.lookupBilibiliLocation(ctx, ip, primary)
	}
	return ""
}

func shouldUseIpInfoFallback(primary *ipApiResponse, info ipInfoResponse) bool {
	if primary == nil || strings.TrimSpace(primary.City) == "" {
		return true
	}
	// If ip-api mapped a CN IP to a direct-controlled municipality (e.g. Unicom HQ in Beijing)
	// while ipinfo disagrees on the province, prefer ipinfo's location.
	if pMun := chineseMunicipalityName(primary.RegionName); pMun != "" {
		infoCityZh := translateEnCityName("CN", info.City)
		infoMun := chineseMunicipalityName(info.Region)
		if infoMun != pMun && infoCityZh != "" {
			return true
		}
	}
	// Keep ip-api's existing Chinese city when ipinfo only has an untranslated ASCII sub-district.
	return containsCJK(translateEnCityName(info.Country, info.City))
}

func (s *IpLocationService) lookupBilibiliLocation(ctx context.Context, ip string, primary *ipApiResponse) string {
	data, err := s.fetchBilibili(ctx, ip)
	if err != nil {
		return ""
	}
	return formatBilibiliLocation(data, primary)
}

func (s *IpLocationService) doGet(ctx context.Context, endpoint string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, ipApiMaxResponseBytes))
	if readErr != nil {
		return resp.StatusCode, resp.Header, nil, readErr
	}
	return resp.StatusCode, resp.Header, body, nil
}

func (s *IpLocationService) fetchSingle(ip string) (ipApiResponse, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ipLocationRequestTimeout)
	defer cancel()

	endpoint := fmt.Sprintf(
		"%s/json/%s?lang=zh-CN&fields=%s",
		s.baseURL(),
		url.PathEscape(ip),
		url.QueryEscape(ipApiFieldsParam),
	)
	status, headers, body, err := s.doGet(ctx, endpoint)
	if err != nil {
		return ipApiResponse{}, false, err
	}
	if s.handleIpApiRateLimit(status, headers) && status == http.StatusTooManyRequests {
		return ipApiResponse{}, true, fmt.Errorf("rate limited (HTTP 429)")
	}
	if status < 200 || status >= 300 {
		return ipApiResponse{}, false, fmt.Errorf("unexpected HTTP status %d", status)
	}
	var parsed ipApiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ipApiResponse{}, false, err
	}
	return parsed, false, nil
}

func (s *IpLocationService) fetchBatchRaw(chunk []string) (map[string]ipApiResponse, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ipLocationRequestTimeout)
	defer cancel()

	payload, err := json.Marshal(chunk)
	if err != nil {
		return nil, false, err
	}

	endpoint := fmt.Sprintf(
		"%s/batch?lang=zh-CN&fields=%s",
		s.baseURL(),
		url.QueryEscape(ipApiFieldsParam),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if s.handleIpApiRateLimit(resp.StatusCode, resp.Header) && resp.StatusCode == http.StatusTooManyRequests {
		return nil, true, fmt.Errorf("rate limited (HTTP 429)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, ipApiMaxResponseBytes))
	if err != nil {
		return nil, false, err
	}
	var items []ipApiResponse
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, false, err
	}

	resolved := make(map[string]ipApiResponse, len(items))
	for i, item := range items {
		if item.Status != "" && item.Status != "success" {
			continue
		}
		key := ""
		if item.Query != "" {
			if c, ok, _ := normalizeQueryIP(item.Query); ok {
				key = c
			}
		}
		if key == "" && i < len(chunk) {
			key = chunk[i]
		}
		if key != "" {
			resolved[key] = item
		}
	}
	return resolved, false, nil
}

func (s *IpLocationService) fetchBilibili(ctx context.Context, ip string) (bilibiliIpData, error) {
	base := s.bilibiliURL()
	if base == "" {
		return bilibiliIpData{}, fmt.Errorf("bilibili source disabled")
	}
	s.mu.RLock()
	inCooldown := s.now().Before(s.bilibiliCooldown)
	s.mu.RUnlock()
	if inCooldown {
		return bilibiliIpData{}, fmt.Errorf("bilibili rate limit cooldown")
	}

	reqCtx, cancel := context.WithTimeout(ctx, ipLocationCalibrateSubLimit)
	defer cancel()

	endpoint := fmt.Sprintf("%s/ip_service/v1/ip_service/get_ip_addr?ip=%s", base, url.QueryEscape(ip))
	status, _, body, err := s.doGet(reqCtx, endpoint)
	if err != nil {
		return bilibiliIpData{}, err
	}
	if status == http.StatusTooManyRequests || status == http.StatusForbidden || status == http.StatusPreconditionFailed {
		s.extendCooldown(&s.bilibiliCooldown, bilibiliRateLimitDefault)
		return bilibiliIpData{}, fmt.Errorf("bilibili rate limited (HTTP %d)", status)
	}
	if status < 200 || status >= 300 {
		return bilibiliIpData{}, fmt.Errorf("bilibili HTTP %d", status)
	}

	var parsed bilibiliIpResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return bilibiliIpData{}, err
	}
	if parsed.Code == -412 || parsed.Code == -509 || parsed.Code == -429 {
		s.extendCooldown(&s.bilibiliCooldown, bilibiliRateLimitDefault)
		return bilibiliIpData{}, fmt.Errorf("bilibili code %d", parsed.Code)
	}
	if parsed.Code != 0 {
		return bilibiliIpData{}, fmt.Errorf("bilibili API error code %d", parsed.Code)
	}
	return parsed.Data, nil
}

func (s *IpLocationService) fetchIpInfo(ctx context.Context, ip string) (ipInfoResponse, error) {
	base := s.ipinfoURL()
	if base == "" {
		return ipInfoResponse{}, fmt.Errorf("ipinfo source disabled")
	}
	s.mu.RLock()
	inCooldown := s.now().Before(s.ipinfoCooldown)
	s.mu.RUnlock()
	if inCooldown {
		return ipInfoResponse{}, fmt.Errorf("ipinfo rate limit cooldown")
	}

	reqCtx, cancel := context.WithTimeout(ctx, ipLocationCalibrateSubLimit)
	defer cancel()

	endpoint := fmt.Sprintf("%s/%s/json", base, url.PathEscape(ip))
	status, _, body, err := s.doGet(reqCtx, endpoint)
	if err != nil {
		return ipInfoResponse{}, err
	}
	if status == http.StatusTooManyRequests {
		s.extendCooldown(&s.ipinfoCooldown, ipLocationRateLimitDefault)
		return ipInfoResponse{}, fmt.Errorf("ipinfo rate limited (HTTP 429)")
	}
	if status < 200 || status >= 300 {
		return ipInfoResponse{}, fmt.Errorf("ipinfo HTTP %d", status)
	}

	var parsed ipInfoResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ipInfoResponse{}, err
	}
	return parsed, nil
}

func (s *IpLocationService) extendCooldown(target *time.Time, ttl time.Duration) {
	s.mu.Lock()
	until := s.now().Add(ttl)
	if until.After(*target) {
		*target = until
	}
	s.mu.Unlock()
}

func (s *IpLocationService) handleIpApiRateLimit(statusCode int, headers http.Header) bool {
	remaining := strings.TrimSpace(headers.Get("X-Rl"))
	if statusCode != http.StatusTooManyRequests && remaining != "0" {
		return false
	}

	ttl := ipLocationRateLimitDefault
	if rawTTL := strings.TrimSpace(headers.Get("X-Ttl")); rawTTL != "" {
		if secs, err := strconv.Atoi(rawTTL); err == nil && secs > 0 {
			ttl = time.Duration(secs) * time.Second
		}
	}

	s.extendCooldown(&s.cooldownUntil, ttl)
	return true
}

func (s *IpLocationService) putCache(entries map[string]string) {
	if len(entries) == 0 {
		return
	}
	now := s.now()
	expiresAt := now.Add(ipLocationCacheTTL)

	s.mu.Lock()
	defer s.mu.Unlock()

	for ip, loc := range entries {
		if loc == "" {
			continue
		}
		if _, exists := s.cache[ip]; !exists {
			if len(s.cache) >= ipLocationMaxCacheEntries {
				s.evictLocked(now)
			}
			s.order = append(s.order, ip)
		}
		s.cache[ip] = ipLocationCacheEntry{
			location:  loc,
			expiresAt: expiresAt,
		}
	}
}

func (s *IpLocationService) evictLocked(now time.Time) {
	kept := s.order[:0]
	for _, key := range s.order {
		entry, ok := s.cache[key]
		if !ok || !now.Before(entry.expiresAt) {
			delete(s.cache, key)
			continue
		}
		kept = append(kept, key)
	}
	s.order = kept

	if len(s.cache) < ipLocationMaxCacheEntries {
		return
	}

	// Still full after purging expired keys: drop the oldest 10% FIFO.
	dropCount := max(1, len(s.order)/10)
	if dropCount > len(s.order) {
		dropCount = len(s.order)
	}
	for _, key := range s.order[:dropCount] {
		delete(s.cache, key)
	}
	s.order = append([]string(nil), s.order[dropCount:]...)
}

func normalizeQueryIP(raw string) (canonical string, isPublic bool, privateLabel string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false, ""
	}
	if host, _, splitErr := net.SplitHostPort(trimmed); splitErr == nil {
		trimmed = strings.TrimSpace(host)
	}
	// Xray access logs store bracketed IPv6 without port (e.g. "[2409:...]");
	// strip outer brackets so netip.ParseAddr can parse them.
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		trimmed = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	}
	addr, err := netip.ParseAddr(trimmed)
	if err != nil {
		return "", false, ""
	}
	addr = addr.Unmap().WithZone("")
	if addr.IsLoopback() {
		return addr.String(), false, "本地"
	}
	if addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() || addr.IsUnspecified() || cgnatPrefix.Contains(addr) {
		return addr.String(), false, "内网"
	}
	return addr.String(), true, ""
}

// FormatIpLocation turns an ip-api response into a short "city + ISP" label
// such as "北京移动", "深圳电信", or "苏州联通".
func FormatIpLocation(res ipApiResponse) string {
	if res.Status != "" && res.Status != "success" {
		return ""
	}
	city := formatCityName(res)
	isp := formatIspName(res)
	return combineCityAndIsp(city, isp)
}

func formatCityName(res ipApiResponse) string {
	cc := strings.ToUpper(strings.TrimSpace(res.CountryCode))
	city := strings.TrimSpace(res.City)
	region := strings.TrimSpace(res.RegionName)
	country := strings.TrimSpace(res.Country)

	switch cc {
	case "HK":
		return "香港"
	case "MO":
		return "澳门"
	case "SG":
		return "新加坡"
	case "CN", "TW":
		if cc == "CN" {
			if mun := chineseMunicipalityName(region); mun != "" {
				return mun
			}
			if mun := chineseMunicipalityName(city); mun != "" {
				return mun
			}
		}
		if city != "" {
			return trimChineseCitySuffix(city, cc == "TW")
		}
		if region != "" {
			return trimChineseRegionSuffix(region)
		}
		if cc == "TW" {
			return "台湾"
		}
		if country != "" {
			return country
		}
		return "中国"
	case "JP":
		if !containsCJK(city) {
			switch region {
			case "东京都", "東京都":
				return "东京"
			case "大阪府":
				return "大阪"
			case "京都府":
				return "京都"
			}
		}
	}

	if city != "" {
		if containsCJK(city) {
			return trimChineseCitySuffix(city, false)
		}
		return city
	}
	if region != "" {
		return region
	}
	return country
}

func trimChineseCitySuffix(city string, trimCounty bool) string {
	runes := []rune(city)
	if len(runes) > 2 {
		if strings.HasSuffix(city, "市") {
			return string(runes[:len(runes)-1])
		}
		if trimCounty && (strings.HasSuffix(city, "县") || strings.HasSuffix(city, "縣")) {
			return string(runes[:len(runes)-1])
		}
	}
	return city
}

func trimChineseRegionSuffix(region string) string {
	for _, suffix := range []string{"自治区", "特别行政区", "省", "市"} {
		if strings.HasSuffix(region, suffix) {
			trimmed := strings.TrimSuffix(region, suffix)
			if len([]rune(trimmed)) >= 2 {
				return trimmed
			}
		}
	}
	return region
}

func chineseMunicipalityName(region string) string {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "北京", "北京市", "beijing", "beijing municipality":
		return "北京"
	case "上海", "上海市", "shanghai", "shanghai municipality":
		return "上海"
	case "天津", "天津市", "tianjin", "tianjin municipality":
		return "天津"
	case "重庆", "重庆市", "重慶", "重慶市", "chongqing", "chongqing municipality":
		return "重庆"
	default:
		return ""
	}
}

var mobileASNs = map[string]bool{
	"AS9808": true, "AS56040": true, "AS56041": true, "AS56042": true,
	"AS56044": true, "AS56046": true, "AS56047": true, "AS56048": true,
	"AS24400": true, "AS24444": true, "AS24445": true, "AS24547": true,
	"AS24138": true, "AS9231": true, "AS58453": true, "AS9394": true,
}

var telecomASNs = map[string]bool{
	"AS4134": true, "AS4809": true, "AS23764": true, "AS4812": true,
	"AS4813": true, "AS4816": true, "AS4847": true, "AS17638": true,
	"AS17775": true, "AS23650": true, "AS36678": true, "AS58543": true,
	"AS134756": true, "AS134760": true, "AS134762": true, "AS134763": true,
	"AS134764": true, "AS134765": true, "AS134766": true, "AS134767": true,
	"AS134768": true, "AS134769": true, "AS134770": true, "AS134771": true,
	"AS134772": true, "AS134773": true, "AS134774": true, "AS134775": true,
	"AS140061": true, "AS140633": true, "AS140636": true, "AS140638": true,
}

var unicomASNs = map[string]bool{
	"AS4837": true, "AS9929": true, "AS10099": true, "AS4808": true,
	"AS17621": true, "AS17622": true, "AS17623": true, "AS17816": true,
	"AS140726": true, "AS140979": true, "AS134542": true, "AS134543": true,
}

var broadnetASNs = map[string]bool{
	"AS139007": true, "AS142317": true, "AS7641": true, "AS133775": true, "AS24139": true,
}

var eduNetASNs = map[string]bool{
	"AS4538": true, "AS24348": true, "AS24349": true, "AS24350": true, "AS23910": true, "AS23911": true,
}

var techNetASNs = map[string]bool{
	"AS7497": true, "AS9800": true,
}

var drPengASNs = map[string]bool{
	"AS17964": true, "AS17962": true,
}

func extractASNumber(asField string) string {
	fields := strings.Fields(asField)
	if len(fields) == 0 {
		return ""
	}
	first := strings.ToUpper(fields[0])
	if strings.HasPrefix(first, "AS") && len(first) > 2 {
		return first
	}
	return ""
}

func formatIspName(res ipApiResponse) string {
	asNum := extractASNumber(res.AS)
	combined := strings.ToLower(strings.Join([]string{res.ISP, res.Org, res.AS}, " "))
	cc := strings.ToUpper(strings.TrimSpace(res.CountryCode))

	switch {
	case mobileASNs[asNum] || containsAny(combined, "china mobile", "cmcc", "cmnet", "cmhk", "mobile communications", "中国移动"):
		return "移动"
	case telecomASNs[asNum] || containsAny(combined, "chinanet", "china telecom", "chinatelecom", "ctnet", "ctgnet", "中国电信"):
		return "电信"
	case unicomASNs[asNum] || containsAny(combined, "china unicom", "chinaunicom", "unicom", "china169", "cncgroup", "cnc group", "china netcom", "cuii", "中国联通"):
		return "联通"
	case broadnetASNs[asNum] || containsAny(combined, "china broadcasting", "topway", "gehua", "oriental cable", "中国广电", "天威", "歌华"):
		return "广电"
	case eduNetASNs[asNum] || containsAny(combined, "cernet", "education and research", "教育网"):
		return "教育网"
	case techNetASNs[asNum] || containsAny(combined, "cstnet", "科技网"):
		return "科技网"
	case containsAny(combined, "tietong", "railcom", "铁通"):
		return "铁通"
	case drPengASNs[asNum] || containsAny(combined, "dr.peng", "drpeng", "gwbn", "great wall broadband", "长城", "长宽", "鹏博士"):
		return "长宽"
	}

	if cc == "CN" {
		switch {
		case containsAny(combined, "telecom", "电信"):
			return "电信"
		case containsAny(combined, "mobile", "移动"):
			return "移动"
		case containsAny(combined, "netcom", "联通"):
			return "联通"
		case containsAny(combined, "broadcast", "广电"):
			return "广电"
		}
	}

	switch {
	case containsAny(combined, "alibaba", "aliyun", "alicloud", "taobao") || asNum == "AS37963" || asNum == "AS45102":
		return "阿里云"
	case containsAny(combined, "tencent", "qcloud", "dnspod") || asNum == "AS132203" || asNum == "AS45090":
		return "腾讯云"
	case containsAny(combined, "huawei") || asNum == "AS136907":
		return "华为云"
	case containsAny(combined, "bytedance", "volcengine"):
		return "字节"
	case containsAny(combined, "baidu") || asNum == "AS55967":
		return "百度"
	case containsAny(combined, "chunghwa", "hinet", "cht.com.tw") || asNum == "AS3462":
		return "中华电信"
	case containsAny(combined, "taiwan mobile") || asNum == "AS24158":
		return "台湾大哥大"
	case containsAny(combined, "far eastone", "fetnet") || asNum == "AS4780":
		return "远传"
	case containsAny(combined, "pccw", "netvigator") || asNum == "AS4760" || hasWord(combined, "hkt"):
		return "HKT"
	case containsAny(combined, "hkbn") || asNum == "AS9269":
		return "HKBN"
	case containsAny(combined, "hutchison global") || asNum == "AS9304" || hasWord(combined, "hgc"):
		return "HGC"
	case containsAny(combined, "cloudflare") || asNum == "AS13335":
		return "Cloudflare"
	case containsAny(combined, "google") || asNum == "AS15169" || asNum == "AS396982":
		return "Google"
	case containsAny(combined, "amazon", "aws") || asNum == "AS16509" || asNum == "AS14618":
		return "AWS"
	case containsAny(combined, "microsoft", "azure") || asNum == "AS8075":
		return "Azure"
	case containsAny(combined, "oracle") || asNum == "AS31898":
		return "Oracle"
	}

	raw := strings.TrimSpace(res.ISP)
	if raw == "" {
		raw = strings.TrimSpace(res.Org)
	}
	if raw == "" && res.AS != "" {
		if asNum != "" {
			raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(res.AS, asNum), strings.ToLower(asNum)))
		} else {
			raw = strings.TrimSpace(res.AS)
		}
	}
	return shortenIspName(raw)
}

func shortenIspName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Trim common corporate suffixes to keep badge width compact.
	suffixes := []string{
		", Inc.", " Inc.", ", Inc", " Inc",
		", LLC", " LLC",
		", Ltd.", " Ltd.", ", Ltd", " Ltd",
		" Co., Ltd.", " Co.,Ltd.", " Co. Ltd.", " Co.,",
		" Limited", " Corporation", " Corp.", " Corp",
		" Pte.", " Pte", " GmbH", " B.V.", " S.A.",
	}
	for {
		changed := false
		for _, suffix := range suffixes {
			if len(s) > len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix) {
				s = strings.TrimSpace(s[:len(s)-len(suffix)])
				s = strings.TrimRight(s, ".,; ")
				changed = true
				break
			}
		}
		if !changed {
			break
		}
	}
	runes := []rune(s)
	if len(runes) > 18 {
		return strings.TrimSpace(string(runes[:18]))
	}
	return s
}

func combineCityAndIsp(city, isp string) string {
	if city == "" {
		return isp
	}
	if isp == "" {
		return city
	}
	cityRunes := []rune(city)
	ispRunes := []rune(isp)
	lastCity := cityRunes[len(cityRunes)-1]
	firstIsp := ispRunes[0]
	// CJK + CJK joins tightly ("北京移动"); ASCII edges get a separating space ("香港 HKT", "Frankfurt AWS").
	if lastCity > unicode.MaxASCII && firstIsp > unicode.MaxASCII {
		return city + isp
	}
	return city + " " + isp
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func hasWord(s, word string) bool {
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if f == word {
			return true
		}
	}
	return false
}

func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han) {
			return true
		}
	}
	return false
}

var cnIPv6Prefixes = []netip.Prefix{
	netip.MustParsePrefix("2408::/20"),
	netip.MustParsePrefix("2409:8000::/20"),
	netip.MustParsePrefix("240e::/20"),
}

func looksLikeCNIP(canonical string) bool {
	addr, err := netip.ParseAddr(canonical)
	if err != nil {
		return false
	}
	for _, prefix := range cnIPv6Prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func isPlaceholderLocation(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || trimmed == "*" || trimmed == "-" {
		return true
	}
	lower := strings.ToLower(trimmed)
	return containsAny(lower, "保留", "内网", "局域网", "未知", "iana", "private", "reserved")
}

func formatBilibiliLocation(data bilibiliIpData, fallbackRes *ipApiResponse) string {
	country := strings.TrimSpace(data.Country)
	if country != "中国" {
		return ""
	}
	prov := strings.TrimSpace(data.Province)
	city := strings.TrimSpace(data.City)

	locCity := ""
	switch {
	case strings.HasPrefix(prov, "香港"):
		locCity = "香港"
	case strings.HasPrefix(prov, "澳门"), strings.HasPrefix(prov, "澳門"):
		locCity = "澳门"
	case prov == "台湾" || prov == "台灣":
		if !isPlaceholderLocation(city) {
			locCity = trimChineseCitySuffix(city, true)
		} else {
			locCity = "台湾"
		}
	default:
		if mun := chineseMunicipalityName(prov); mun != "" {
			locCity = mun
		} else if !isPlaceholderLocation(city) {
			locCity = trimChineseCitySuffix(city, false)
		} else if !isPlaceholderLocation(prov) {
			provShort := trimChineseRegionSuffix(prov)
			if fallbackRes != nil &&
				trimChineseRegionSuffix(strings.TrimSpace(fallbackRes.RegionName)) == provShort &&
				strings.TrimSpace(fallbackRes.City) != "" {
				locCity = trimChineseCitySuffix(strings.TrimSpace(fallbackRes.City), false)
			} else {
				locCity = provShort
			}
		}
	}
	if locCity == "" {
		return ""
	}
	isp := normalizeBilibiliIsp(data.ISP, fallbackRes)
	return combineCityAndIsp(locCity, isp)
}

func normalizeBilibiliIsp(raw string, fallbackRes *ipApiResponse) string {
	isp := strings.TrimSpace(raw)
	if isPlaceholderLocation(isp) || strings.EqualFold(isp, "bgp") || isp == "多线" {
		isp = ""
	}
	if isp != "" {
		lower := strings.ToLower(isp)
		// Match cloud vendors before generic carrier tokens because Bilibili
		// multi-line BGP strings look like "阿里云/电信/联通/移动/教育网".
		switch {
		case containsAny(lower, "阿里云", "alibaba", "aliyun"):
			return "阿里云"
		case containsAny(lower, "腾讯云", "腾讯", "tencent"):
			return "腾讯云"
		case containsAny(lower, "华为云", "华为", "huawei"):
			return "华为云"
		case containsAny(lower, "火山", "字节", "bytedance"):
			return "字节"
		case containsAny(lower, "百度云", "百度", "baidu"):
			return "百度"
		case containsAny(lower, "京东云"):
			return "京东云"
		case containsAny(lower, "金山云"):
			return "金山云"
		case containsAny(lower, "天翼云"):
			return "天翼云"
		case containsAny(lower, "移动云"):
			return "移动云"
		case containsAny(lower, "联通云"):
			return "联通云"
		case containsAny(lower, "ucloud", "优刻得"):
			return "UCloud"
		}
		if mapped := formatIspName(ipApiResponse{CountryCode: "CN", ISP: isp}); mapped != "" {
			return mapped
		}
	}
	if fallbackRes != nil {
		return formatIspName(*fallbackRes)
	}
	return ""
}

// FormatIpInfoLocation converts an ipinfo.io JSON payload into a concise "city + ISP" label.
func FormatIpInfoLocation(res ipInfoResponse) string {
	cc := strings.ToUpper(strings.TrimSpace(res.Country))
	city := translateEnCityName(cc, res.City)
	mapped := ipApiResponse{
		Status:      "success",
		CountryCode: cc,
		RegionName:  res.Region,
		City:        city,
		Org:         res.Org,
		AS:          res.Org,
		Query:       res.IP,
	}
	return FormatIpLocation(mapped)
}

var enCityToZhByCC = map[string]map[string]string{
	"CN": {
		"beijing":     "北京",
		"shanghai":    "上海",
		"tianjin":     "天津",
		"chongqing":   "重庆",
		"guangzhou":   "广州",
		"shenzhen":    "深圳",
		"dongguan":    "东莞",
		"foshan":      "佛山",
		"zhuhai":      "珠海",
		"huizhou":     "惠州",
		"zhongshan":   "中山",
		"nanjing":     "南京",
		"suzhou":      "苏州",
		"wuxi":        "无锡",
		"changzhou":   "常州",
		"xuzhou":      "徐州",
		"nantong":     "南通",
		"hangzhou":    "杭州",
		"ningbo":      "宁波",
		"wenzhou":     "温州",
		"chengdu":     "成都",
		"wuhan":       "武汉",
		"xi'an":       "西安",
		"xian":        "西安",
		"changsha":    "长沙",
		"zhengzhou":   "郑州",
		"jinan":       "济南",
		"qingdao":     "青岛",
		"dalian":      "大连",
		"shenyang":    "沈阳",
		"harbin":      "哈尔滨",
		"changchun":   "长春",
		"kunming":     "昆明",
		"fuzhou":      "福州",
		"xiamen":      "厦门",
		"quanzhou":    "泉州",
		"hefei":       "合肥",
		"nanchang":    "南昌",
		"nanning":     "南宁",
		"guiyang":     "贵阳",
		"taiyuan":     "太原",
		"shijiazhuang": "石家庄",
		"lanzhou":     "兰州",
		"haikou":      "海口",
		"sanya":       "三亚",
		"urumqi":      "乌鲁木齐",
		"hohhot":      "呼和浩特",
		"yinchuan":    "银川",
		"xining":      "西宁",
		"lhasa":       "拉萨",
	},
	"TW": {
		"taipei":    "台北",
		"taichung":  "台中",
		"kaohsiung": "高雄",
		"tainan":    "台南",
	},
	"JP": {
		"tokyo": "东京",
		"osaka": "大阪",
		"kyoto": "京都",
	},
}

func translateEnCityName(cc, city string) string {
	trimmed := strings.TrimSpace(city)
	if trimmed == "" {
		return ""
	}
	if byCity, ok := enCityToZhByCC[strings.ToUpper(strings.TrimSpace(cc))]; ok {
		if zh, found := byCity[strings.ToLower(trimmed)]; found {
			return zh
		}
	}
	return trimmed
}
