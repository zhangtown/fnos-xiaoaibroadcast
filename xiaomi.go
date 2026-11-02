// 小米云（MIoT）客户端：登录、取设备、让小爱音箱说出指定文字。
//
// 协议实现参照 miservice 3.0.1（miaccount.py / miioservice.py）逐条移植，
// 只是把 asyncio 换成同步 net/http，把 token 存进本应用的 config.json。
//
// 三条链路：
//   1. 密码登录  account.xiaomi.com/pass/serviceLogin + serviceLoginAuth2
//   2. 换票      location&clientSign=... → Set-Cookie: serviceToken
//   3. 调设备    https://api.io.mi.com/app/miotspec/action（带 HMAC 签名）
package main

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	accountBase = "https://account.xiaomi.com"
	miioBase    = "https://api.io.mi.com/app"

	uaLogin = "APP/com.xiaomi.mihome APPV/11.3.203 iosPassportSDK/4.2.50 iOS/26.3.1 MK/aVBob25lMTcsMg== DEVT/aVBob25l DEVS/aU9T BRA/QXBwbGU= L/zh_CN"
	uaMiio  = "iOS-14.4-6.0.103-iPhone12,3--D7744744F7AF32F0544445285880DD63E47D9BE9-8816080-84A3F44E137B71AE-iPhone"
)

// 小爱音箱（L05C 等 wifispeaker）的「朗读文字」动作在 MIoT 规范里的编号。
// 见 https://miot-spec.org/miot-spec-v2/instance?type=urn:miot-spec-v2:device:speaker:0000A015:xiaomi-l05c:2
//   2-1 音量（uint8）、5-3 Play Text（1 个 string 入参）
const (
	sidVolume = 2
	piidVolume = 1
	sidSpeaker = 5
	aiidPlayText = 3
)

var (
	errNeedLogin = errors.New("小米登录已失效，请在「账号与设备」里重新输入密码登录")
)

// ---------- 简易 cookie 罐（net/http 自带 jar 不方便取 serviceToken 值） ----------

type cookieJar struct {
	mu sync.Mutex
	m  map[string]string
}

func newCookieJar() *cookieJar { return &cookieJar{m: map[string]string{}} }

func (j *cookieJar) set(k, v string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.m[k] = v
}

func (j *cookieJar) get(k string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.m[k]
}

func (j *cookieJar) header() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(j.m))
	for k, v := range j.m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

func (j *cookieJar) absorb(h http.Header) {
	for _, line := range h.Values("Set-Cookie") {
		seg := strings.SplitN(line, ";", 2)[0]
		kv := strings.SplitN(strings.TrimSpace(seg), "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		j.set(kv[0], strings.Trim(kv[1], `"`))
	}
}

// ---------- 客户端 ----------

type XM struct {
	hc  *http.Client
	jar *cookieJar
	st  *Store
	mu  sync.Mutex // 串行化「刷新票 → 发请求」以免同时刷新
}

func NewXM(st *Store) *XM {
	x := &XM{hc: &http.Client{Timeout: 25 * time.Second}, jar: newCookieJar(), st: st}
	x.hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("重定向过多")
		}
		if req.Response != nil {
			x.jar.absorb(req.Response.Header)
		}
		if ck := x.jar.header(); ck != "" {
			req.Header.Set("Cookie", ck)
		}
		return nil
	}
	return x
}

func (x *XM) do(method, u string, form url.Values, ua string, extra map[string]string) ([]byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	if ck := x.jar.header(); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	resp, err := x.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	x.jar.absorb(resp.Header)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return raw, &apiError{Status: resp.StatusCode, Body: string(raw), Wait: parseWaitSecond(string(raw))}
	}
	return raw, nil
}

// apiError 保留 HTTP 状态与小米要求的等待秒数（限流时是 429 + wait_second）。
type apiError struct {
	Status int
	Body   string
	Wait   int
}

func (e *apiError) Error() string {
	if e.Wait > 0 {
		return fmt.Sprintf("HTTP %d（小米要求等 %d 秒）：%s", e.Status, e.Wait, truncate(e.Body, 200))
	}
	return fmt.Sprintf("HTTP %d：%s", e.Status, truncate(e.Body, 300))
}

var waitRe = regexp.MustCompile(`"wait_second"\s*:\s*(\d+)`)

func parseWaitSecond(body string) int {
	if m := waitRe.FindStringSubmatch(body); len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// refreshLocked 用 passToken 续期（不需要密码）。调用前需持有 x.mu。
func (x *XM) refreshLocked() error {
	a := x.st.Get().Account
	fail := func(err error) error {
		_ = x.st.Update(func(c *Config) error {
			c.Account.ServiceToken = ""
			c.Account.LastAuthError = err.Error()
			return nil
		})
		return err
	}
	if a.PassToken == "" || a.UserID == "" {
		return fail(errNeedLogin)
	}
	x.jar = newCookieJar()
	x.jar.set("sdkVersion", "3.9")
	x.jar.set("deviceId", a.DeviceID)
	x.jar.set("userId", a.UserID)
	x.jar.set("passToken", a.PassToken)

	raw, err := x.do("GET", accountBase+"/pass/serviceLogin?sid=xiaomiio&_json=true", nil, uaLogin, nil)
	if err != nil {
		return fmt.Errorf("续期失败（网络）：%w", err)
	}
	r, err := parseXMJSON(raw)
	if err != nil {
		return fmt.Errorf("续期失败（解析）：%w", err)
	}
	if getInt(r, "code") != 0 || getStr(r, "location") == "" {
		return fail(errNeedLogin)
	}
	if err := x.stsLocked(r); err != nil {
		return fail(err)
	}
	a.SSecurity = getStr(r, "ssecurity")
	if uid := getStr(r, "userId"); uid != "" {
		a.UserID = uid
	}
	if pt := getStr(r, "passToken"); pt != "" {
		a.PassToken = pt
	}
	a.ServiceToken = x.jar.get("serviceToken")
	a.TokenAt = time.Now().Unix()
	a.LastAuthError = ""
	return x.st.Update(func(c *Config) error {
		c.Account = a
		return nil
	})
}

// stsLocked 拿 location + clientSign 换 serviceToken（写进 cookie 罐）。
func (x *XM) stsLocked(r map[string]any) error {
	location := getStr(r, "location")
	nonce := getStr(r, "nonce")
	ssecurity := getStr(r, "ssecurity")
	if location == "" || nonce == "" || ssecurity == "" {
		return errors.New("登录响应缺少 location/nonce/ssecurity，登录未完成")
	}
	sum := sha1.Sum([]byte("nonce=" + nonce + "&" + ssecurity))
	clientSign := base64.StdEncoding.EncodeToString(sum[:])
	if _, err := x.do("GET", location+"&clientSign="+url.QueryEscape(clientSign), nil, uaLogin, nil); err != nil {
		return fmt.Errorf("换取 serviceToken 失败：%w", err)
	}
	if x.jar.get("serviceToken") == "" {
		return errors.New("没拿到 serviceToken（小米未下发票据）")
	}
	return nil
}

// EnsureToken 确保手里有 serviceToken。
func (x *XM) EnsureToken() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	a := x.st.Get().Account
	if a.ServiceToken != "" && a.SSecurity != "" {
		return nil
	}
	return x.refreshLocked()
}

// miRequest 发一个带签名的小米云请求，遇到授权失效会自动续期重试一次。
func (x *XM) miRequest(uri string, payload any) (json.RawMessage, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.miRequestLocked(uri, payload, true)
}

func (x *XM) miRequestLocked(uri string, payload any, retry bool) (json.RawMessage, error) {
	a := x.st.Get().Account
	if a.ServiceToken == "" {
		if err := x.refreshLocked(); err != nil {
			return nil, err
		}
		a = x.st.Get().Account
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	form := signData(uri, string(data), a.SSecurity)

	x.jar.set("userId", a.UserID)
	x.jar.set("serviceToken", a.ServiceToken)
	x.jar.set("PassportDeviceId", a.DeviceID)

	raw, err := x.do("POST", miioBase+uri, form, uaMiio, map[string]string{
		"x-xiaomi-protocal-flag-cli": "PROTOCAL-HTTP2",
	})
	if err != nil {
		// 小米限流（429 client withdraw）会带 wait_second：按它说的等，然后重试一次
		var ae *apiError
		if errors.As(err, &ae) && (ae.Status == 429 || ae.Status == 503) && retry {
			wait := ae.Wait
			if wait <= 0 {
				wait = 5
			}
			if wait > 90 {
				wait = 90
			}
			x.st.AddLog("系统", "", true, fmt.Sprintf("小米限流（429），按它要求等 %d 秒后重试", wait))
			time.Sleep(time.Duration(wait) * time.Second)
			return x.miRequestLocked(uri, payload, false)
		}
		return nil, fmt.Errorf("请求小米云失败：%w", err)
	}

	var env struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("小米云响应不是 JSON：%s", truncate(string(raw), 300))
	}
	if env.Code == 0 {
		return env.Result, nil
	}
	if retry && isAuthErr(env.Code, env.Message) {
		_ = x.st.Update(func(c *Config) error {
			c.Account.LastAuthError = fmt.Sprintf("code=%d %s", env.Code, env.Message)
			return nil
		})
		if err := x.refreshLocked(); err == nil {
			return x.miRequestLocked(uri, payload, false)
		}
	}
	return nil, fmt.Errorf("小米云返回 code=%d %s", env.Code, strings.TrimSpace(env.Message))
}

// Devices 拉取账号下的设备，筛出音箱类。
func (x *XM) Devices() ([]Device, error) {
	res, err := x.miRequest("/home/device_list", map[string]any{
		"getVirtualModel": false, "getHuamiDevices": 0,
	})
	if err != nil {
		return nil, err
	}
	var d struct {
		List []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
			DID   string `json:"did"`
		} `json:"list"`
	}
	if err := json.Unmarshal(res, &d); err != nil {
		return nil, fmt.Errorf("设备列表解析失败：%s", truncate(string(res), 300))
	}
	out := make([]Device, 0, len(d.List))
	all := make([]Device, 0, len(d.List))
	for _, it := range d.List {
		dev := Device{Name: it.Name, Model: it.Model, DID: it.DID}
		all = append(all, dev)
		if looksLikeSpeaker(dev) {
			out = append(out, dev)
		}
	}
	if len(out) == 0 {
		out = all
	}
	return out, nil
}

func looksLikeSpeaker(d Device) bool {
	m := strings.ToLower(d.Model)
	if strings.Contains(m, "speaker") || strings.Contains(m, "sound") || strings.Contains(m, "box") {
		return true
	}
	return strings.Contains(d.Name, "音箱") || strings.Contains(d.Name, "小爱") || strings.Contains(d.Name, "触屏")
}

// Say 让指定音箱朗读一段文字；volume>0 时先把音量调到该值。
func (x *XM) Say(did, text string, volume int) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("文案是空的")
	}
	if did == "" {
		return errors.New("还没选音箱")
	}
	if volume > 0 {
		if volume > 100 {
			volume = 100
		}
		res, err := x.miRequest("/miotspec/prop/set", map[string]any{
			"params": []map[string]any{{
				"did": did, "siid": sidVolume, "piid": piidVolume, "value": volume,
			}},
		})
		if err != nil {
			return fmt.Errorf("设置音量失败：%w", err)
		}
		// prop/set 的 result 是数组：[{"code":0}]
		var arr []struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(res, &arr) == nil && len(arr) > 0 && arr[0].Code != 0 {
			return fmt.Errorf("设置音量被拒绝（code=%d）", arr[0].Code)
		}
		time.Sleep(300 * time.Millisecond)
	}

	res, err := x.miRequest("/miotspec/action", map[string]any{
		"params": map[string]any{
			"did": did, "siid": sidSpeaker, "aiid": aiidPlayText, "in": []string{text},
		},
	})
	if err != nil {
		return err
	}
	var one struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(res, &one); err != nil {
		return fmt.Errorf("播报响应异常：%s", truncate(string(res), 300))
	}
	if one.Code != 0 {
		return fmt.Errorf("音箱拒绝朗读（code=%d，可能离线/静音/免打扰）", one.Code)
	}
	return nil
}

// ---------- 签名 ----------

func signNonce(ssecurity, nonce string) (string, error) {
	ss, err := base64.StdEncoding.DecodeString(ssecurity)
	if err != nil {
		return "", err
	}
	nn, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(ss)
	h.Write(nn)
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

func newNonce() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf[:8]); err != nil {
		stamp := time.Now().UnixNano()
		binary.BigEndian.PutUint64(buf[:8], uint64(stamp))
	}
	binary.BigEndian.PutUint32(buf[8:], uint32(time.Now().Unix()/60))
	return base64.StdEncoding.EncodeToString(buf)
}

func signData(uri, data, ssecurity string) url.Values {
	nonce := newNonce()
	snonce, err := signNonce(ssecurity, nonce)
	if err != nil {
		// ssecurity 不合法时留一个明显会失败但不会 panic 的值
		snonce = ""
	}
	key, _ := base64.StdEncoding.DecodeString(snonce)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(uri + "&" + snonce + "&" + nonce + "&data=" + data))
	return url.Values{
		"_nonce":    {nonce},
		"data":      {data},
		"signature": {base64.StdEncoding.EncodeToString(mac.Sum(nil))},
	}
}

// ---------- 小工具 ----------

func parseXMJSON(raw []byte) (map[string]any, error) {
	s := strings.TrimSpace(string(raw))
	s = strings.TrimPrefix(s, "&&&START&&&")
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("%w（原文：%s）", err, truncate(s, 200))
	}
	return m, nil
}

func getStr(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		switch t := v.(type) {
		case string:
			return t
		case json.Number:
			return t.String()
		}
	}
	return ""
}

func getInt(m map[string]any, k string) int {
	switch t := m[k].(type) {
	case float64:
		return int(t)
	case string:
		var n int
		fmt.Sscanf(t, "%d", &n)
		return n
	}
	return 0
}

func hexMD5(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomDeviceID() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var b strings.Builder
	for i := 0; i < 16; i++ {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String()
}

func isAuthErr(code int, msg string) bool {
	if code == -1 || code == 401 {
		return true
	}
	m := strings.ToLower(msg)
	return strings.Contains(m, "auth") || strings.Contains(m, "登录") || strings.Contains(m, "token") || strings.Contains(m, "invalid")
}

func loginError(code int, desc string) error {
	switch code {
	case 70016:
		return fmt.Errorf("账号或密码不对（小米返回 70016：%s）", orDefault(desc, "登录验证失败"))
	case 70005:
		return errors.New("密码错误次数过多，账号被临时限制；请在米家 App 里改一次密码或等一会儿再试")
	case 87001:
		return errors.New("需要图形验证码（87001），请先在浏览器/米家 App 里用该账号登录一次再来")
	}
	return fmt.Errorf("登录失败（code=%d）：%s", code, orDefault(desc, "未知原因"))
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
