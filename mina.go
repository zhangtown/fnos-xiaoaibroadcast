// MiNA（小爱语音云）通道：比 MIoT 更"原生"的一条说话路径。
//
// 协议来自 miservice 的 MiNAService：
//   GET  https://api2.mina.mi.com/admin/v2/device_list?master=0&requestId=...   （cookie: userId + micoapi serviceToken）
//   POST https://api2.mina.mi.com/remote/ubus
//        deviceId=<MiNA 的 deviceID>&message={"text":"…"}&method=text_to_speech&path=mibrain&requestId=...
//
// 与 MIoT 的区别：MiNA 只认 cookie（不签名），用的是 sid=micoapi 的票；
// 主机也不同（api2.mina.mi.com），所以 MIoT 被 429 的时候它常常还通。
package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"net/http"
	"strings"
)

const (
	minaBase = "https://api2.mina.mi.com"
	uaMina   = "MiHome/6.0.103 (com.xiaomi.mihome; build:6.0.103.1; iOS 14.4.0) Alamofire/6.0.103 MICO/iOSApp/appStore/6.0.103"
)

// 老版本 miservice 用的是 api.mina.mi.com，这里两个都试（只对 DNS/连接失败换主机）。
var minaBases = []string{minaBase, "https://api.mina.mi.com"}

func (x *XM) minaCall(method, path string, form url.Values) ([]byte, error) {
	var lastErr error
	for _, base := range minaBases {
		raw, err := x.do(method, base+path, form, uaMina, nil)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		msg := err.Error()
		if !strings.Contains(msg, "no such host") && !strings.Contains(msg, "connect") {
			return raw, err
		}
	}
	return nil, lastErr
}

func newRequestID() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	b.WriteString("app_ios_")
	for i := 0; i < 30; i++ {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String()
}

// sidTokenLocked 用 passToken 给任意 sid 换一张票（不需要密码）。
func (x *XM) sidTokenLocked(sid string) (string, string, error) {
	acc := x.st.Get().Account
	if acc.PassToken == "" || acc.UserID == "" {
		return "", "", errNeedLogin
	}
	x.jar = newCookieJar()
	x.jar.set("sdkVersion", "3.9")
	x.jar.set("deviceId", acc.DeviceID)
	x.jar.set("userId", acc.UserID)
	x.jar.set("passToken", acc.PassToken)

	raw, err := x.do("GET", accountBase+"/pass/serviceLogin?sid="+url.QueryEscape(sid)+"&_json=true", nil, uaLogin, nil)
	if err != nil {
		return "", "", fmt.Errorf("换 %s 票失败（网络）：%w", sid, err)
	}
	r, err := parseXMJSON(raw)
	if err != nil {
		return "", "", fmt.Errorf("换 %s 票失败（解析）：%w", sid, err)
	}
	if getInt(r, "code") != 0 {
		return "", "", fmt.Errorf("换 %s 票被拒（code=%d）：小米不认这张 passToken 了", sid, getInt(r, "code"))
	}
	// 有的情况下 serviceLogin 直接下发 serviceToken cookie，不用再走 sts
	if tok := x.jar.get("serviceToken"); tok != "" {
		return getStr(r, "ssecurity"), tok, nil
	}
	if getStr(r, "location") == "" || getStr(r, "nonce") == "" || getStr(r, "ssecurity") == "" {
		return "", "", fmt.Errorf("换 %s 票时小米只回了 code=0、没给 location/nonce/ssecurity（这张 passToken 大概已经不能续票了）", sid)
	}
	if err := x.stsLocked(r); err != nil {
		return "", "", err
	}
	return getStr(r, "ssecurity"), x.jar.get("serviceToken"), nil
}

// ensureMinaLocked 保证手里有 micoapi 的票。
func (x *XM) ensureMinaLocked() error {
	acc := x.st.Get().Account
	if acc.MicoapiToken != "" {
		return nil
	}
	// 1) 优先用 xiaomusic 票据文件里现成的 micoapi 票（它启动时就是用这个 sid 登录的）
	if t, _, err := loadXiaomusicToken(); err == nil && len(t.Micoapi) >= 2 && t.Micoapi[1] != "" {
		return x.st.Update(func(c *Config) error {
			c.Account.MicoapiSec = t.Micoapi[0]
			c.Account.MicoapiToken = t.Micoapi[1]
			if c.Account.UserID == "" {
				c.Account.UserID = t.UserID
			}
			if c.Account.PassToken == "" {
				c.Account.PassToken = t.PassToken
			}
			if c.Account.DeviceID == "" {
				c.Account.DeviceID = t.DeviceID
			}
			return nil
		})
	}
	// 2) 文件里没有，再用 passToken 现换一张
	sec, tok, err := x.sidTokenLocked("micoapi")
	if err != nil {
		return err
	}
	return x.st.Update(func(c *Config) error {
		c.Account.MicoapiSec = sec
		c.Account.MicoapiToken = tok
		return nil
	})
}

type minaDevice struct {
	DeviceID string `json:"deviceID"`
	Name     string `json:"name"`
	Alias    string `json:"alias"`
	Hardware string `json:"hardware"`
}

// minaDevicesLocked 拉 MiNA 设备列表。
func (x *XM) minaDevicesLocked() ([]minaDevice, error) {
	acc := x.st.Get().Account
	x.jar.set("userId", acc.UserID)
	x.jar.set("serviceToken", acc.MicoapiToken)
	raw, err := x.minaCall(http.MethodGet, "/admin/v2/device_list?master=0&requestId="+newRequestID(), nil)
	if err != nil {
		return nil, fmt.Errorf("MiNA 拉设备失败：%w", err)
	}
	var resp struct {
		Code int          `json:"code"`
		Data []minaDevice `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("MiNA 设备列表解析失败：%s", truncate(string(raw), 200))
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("MiNA 设备列表被拒（code=%d）：%s", resp.Code, truncate(string(raw), 160))
	}
	return resp.Data, nil
}

// matchMinaDevice 按名字把 MIoT 设备对应到 MiNA 的 deviceID。
func matchMinaDevice(dev Device, list []minaDevice) string {
	want := strings.TrimSpace(dev.Name)
	for _, d := range list {
		if strings.TrimSpace(d.Alias) == want || strings.TrimSpace(d.Name) == want {
			return d.DeviceID
		}
	}
	for _, d := range list {
		if strings.EqualFold(strings.TrimSpace(d.Hardware), strings.TrimSpace(dev.Model)) {
			return d.DeviceID
		}
	}
	if len(list) == 1 {
		return list[0].DeviceID
	}
	return ""
}


// minaDeviceIDLocked 取当前音箱在 MiNA 侧的 deviceID（第一次用时按名字绑定一次）。
func (x *XM) minaDeviceIDLocked() (string, error) {
	dev := x.st.Get().Device
	devID := strings.TrimSpace(dev.MinaID)
	if devID != "" {
		return devID, nil
	}
	list, err := x.minaDevicesLocked()
	if err != nil {
		return "", err
	}
	var names []string
	for _, d := range list {
		names = append(names, strings.TrimSpace(orDefault(d.Alias, d.Name)))
	}
	devID = matchMinaDevice(dev, list)
	if devID == "" {
		return "", fmt.Errorf("MiNA 里找不到「%s」这台设备（它有：%s）", dev.Name, strings.Join(names, "、"))
	}
	_ = x.st.Update(func(c *Config) error {
		c.Device.MinaID = devID
		return nil
	})
	return devID, nil
}

// ubusCallLocked 发一个 MiNA ubus 调用并返回原始响应；401 时清票重试一次。
func (x *XM) ubusCallLocked(method, path string, msg map[string]any) ([]byte, error) {
	payload, _ := json.Marshal(msg)
	devID, err := x.minaDeviceIDLocked()
	if err != nil {
		return nil, err
	}
	call := func() ([]byte, error) {
		form := url.Values{
			"deviceId":  {devID},
			"message":   {string(payload)},
			"method":    {method},
			"path":      {path},
			"requestId": {newRequestID()},
		}
		acc := x.st.Get().Account
		x.jar.set("userId", acc.UserID)
		x.jar.set("serviceToken", acc.MicoapiToken)
		raw, err := x.minaCall(http.MethodPost, "/remote/ubus", form)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Code int    `json:"code"`
			Msg  string `json:"message"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("MiNA 响应异常：%s", truncate(string(raw), 200))
		}
		if resp.Code != 0 {
			return nil, fmt.Errorf("MiNA 拒绝（code=%d %s）", resp.Code, resp.Msg)
		}
		return raw, nil
	}
	raw, err := call()
	if err != nil && strings.Contains(err.Error(), "HTTP 401") {
		_ = x.st.Update(func(c *Config) error {
			c.Account.MicoapiToken = ""
			return nil
		})
		if e2 := x.ensureMinaLocked(); e2 == nil {
			return call()
		}
	}
	return raw, err
}

// SayViaMina 让小爱用 MiNA 的 text_to_speech 说话。
func (x *XM) SayViaMina(text string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("文案是空的")
	}
	if err := x.ensureMinaLocked(); err != nil {
		return err
	}
	if _, err := x.ubusCallLocked("text_to_speech", "mibrain", map[string]any{"text": text}); err != nil {
		return fmt.Errorf("MiNA 播报失败：%w", err)
	}
	return nil
}

// PlayURL 让小爱播放一段音频（网络地址或 NAS 上的 http 地址）。
func (x *XM) PlayURL(mediaURL string, volume int) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	mediaURL = strings.TrimSpace(mediaURL)
	if mediaURL == "" {
		return fmt.Errorf("音频地址是空的")
	}
	if !strings.HasPrefix(mediaURL, "http://") && !strings.HasPrefix(mediaURL, "https://") {
		return fmt.Errorf("音频地址要以 http:// 或 https:// 开头")
	}
	if err := x.ensureMinaLocked(); err != nil {
		return err
	}
	if volume > 0 {
		v := volume
		if v > 100 {
			v = 100
		}
		_, _ = x.ubusCallLocked("player_set_volume", "mediaplayer", map[string]any{"volume": v, "media": "app_ios"})
	}

	// 小米音箱里有一批型号（含 L05C/LX06 这些）只认 player_play_music：
	// 传一个 music 结构体，而不是简单的一个 url。
	audioID := "1582971365183456177"
	cpID := "355454500"
	music := map[string]any{
		"payload": map[string]any{
			"audio_type": "",
			"audio_items": []any{
				map[string]any{
					"item_id": map[string]any{
						"audio_id": audioID,
						"cp": map[string]any{
							"album_id": "-1", "episode_index": 0, "id": cpID, "name": "xiaowei",
						},
					},
					"stream": map[string]any{"url": mediaURL},
				},
			},
			"list_params": map[string]any{
				"listId": "-1", "loadmore_offset": 0, "origin": "xiaowei", "type": "MUSIC",
			},
		},
		"play_behavior": "REPLACE_ALL",
	}
	musicJSON, _ := json.Marshal(music)
	_, err := x.ubusCallLocked("player_play_music", "mediaplayer", map[string]any{
		"startaudioid": audioID, "music": string(musicJSON),
	})
	if err == nil {
		return nil
	}
	// 老型号走 player_play_url
	if _, err2 := x.ubusCallLocked("player_play_url", "mediaplayer", map[string]any{
		"url": mediaURL, "type": 2, "media": "app_ios",
	}); err2 != nil {
		return fmt.Errorf("音乐播放失败：%v；退回 player_play_url 也失败：%v", err, err2)
	}
	return nil
}

// StopPlaying 停止当前播放（音乐、故事、TTS 都会被打断）。
func (x *XM) StopPlaying() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.ensureMinaLocked(); err != nil {
		return err
	}
	_, err := x.ubusCallLocked("player_play_operation", "mediaplayer", map[string]any{"action": "stop", "media": "app_ios"}); return err
}

// SetVolume 直接把音箱音量设成 v（0-100）。
func (x *XM) SetVolume(v int) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	if err := x.ensureMinaLocked(); err != nil {
		return err
	}
	_, err := x.ubusCallLocked("player_set_volume", "mediaplayer", map[string]any{"volume": v, "media": "app_ios"}); return err
}

// CurrentVolume 读音箱此刻的音量（0-100）。
func (x *XM) CurrentVolume() (int, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.ensureMinaLocked(); err != nil {
		return 0, err
	}
	raw, err := x.ubusCallLocked("player_get_play_status", "mediaplayer", map[string]any{"media": "app_ios"})
	if err != nil {
		return 0, err
	}
	var env struct {
		Data struct {
			Info string `json:"info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, fmt.Errorf("音量响应异常：%s", truncate(string(raw), 200))
	}
	var info struct {
		Volume int `json:"volume"`
	}
	if env.Data.Info != "" {
		_ = json.Unmarshal([]byte(env.Data.Info), &info)
	}
	return info.Volume, nil
}
