// 从 xiaomusic 导入小米登录态。
//
// xiaomusic 把自己的登录票据写在 conf/.mi.token 里（miservice 的 MiTokenStore 格式）：
//
//	{"deviceId":"...","userId":"...","passToken":"...",
//	 "micoapi":["<ssecurity>","<serviceToken>"], "xiaomiio":["<ssecurity>","<serviceToken>"]}
//
// 这个文件就在宿主机上（容器把 /app/conf 挂到了 /vol1/1000/docker/xiaomusic），
// 本应用以 root 跑，直接读得到。导入之后我们用自己的签名去打 MIoT 5-3，
// 和小米云通道完全一样，只是省掉了「密码 + 短信验证」这一步。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

type tokenFile struct {
	DeviceID  string
	UserID    string
	PassToken string
	Micoapi   []string
	Xiaomiio  []string
}

// strOf 容忍「同一个字段有时是字符串有时是数字」（小米的 userId 就是这样）。
func strOf(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	}
	return fmt.Sprint(v)
}

func sidPair(m map[string]any, key string) []string {
	arr, ok := m[key].([]any)
	if !ok || len(arr) < 2 {
		return nil
	}
	return []string{strOf(arr[0]), strOf(arr[1])}
}

var tokenPaths = []string{
	"/vol1/1000/docker/xiaomusic/.mi.token",
	"/vol1/1000/docker/xiaomusic/conf/.mi.token",
	"/vol1/1000/docker/xiaomusic/xiaomusic/.mi.token",
}

// loadXiaomusicToken 找到并解析 xiaomusic 的票据文件。
func loadXiaomusicToken() (*tokenFile, string, error) {
	var lastErr error
	for _, p := range tokenPaths {
		b, err := os.ReadFile(p)
		if err != nil {
			lastErr = err
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			lastErr = fmt.Errorf("%s 不是合法 JSON：%v", p, err)
			continue
		}
		t := &tokenFile{
			DeviceID:  strOf(m["deviceId"]),
			UserID:    strOf(m["userId"]),
			PassToken: strOf(m["passToken"]),
			Micoapi:   sidPair(m, "micoapi"),
			Xiaomiio:  sidPair(m, "xiaomiio"),
		}
		if t.PassToken == "" || t.UserID == "" {
			lastErr = fmt.Errorf("%s 里没有 passToken/userId", p)
			continue
		}
		return t, p, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没找到 .mi.token")
	}
	return nil, "", lastErr
}

// ImportXiaomusicToken 把票据写进本应用，并立刻用一次设备列表验证。
// 成功后自动切到「小米云」通道——之后不再需要 xiaomusic 在跑。
func (a *App) ImportXiaomusicToken() ([]Device, string, error) {
	t, path, err := loadXiaomusicToken()
	if err != nil {
		return nil, "", err
	}
	acc := Account{
		User:      "xiaomusic 导入",
		UserID:    t.UserID,
		PassToken: t.PassToken,
		DeviceID:  t.DeviceID,
		TokenAt:   time.Now().Unix(),
	}
	if len(t.Xiaomiio) >= 2 {
		acc.SSecurity = t.Xiaomiio[0]
		acc.ServiceToken = t.Xiaomiio[1]
	}
	if len(t.Micoapi) >= 2 {
		acc.MicoapiSec = t.Micoapi[0]
		acc.MicoapiToken = t.Micoapi[1]
	}
	if err := a.store.Update(func(c *Config) error {
		c.Account = acc
		return nil
	}); err != nil {
		return nil, path, err
	}

	// 验证：能列出设备说明票据可用；不行就用 passToken 续一次再试
	devs, err := a.xm.Devices()
	if err != nil {
		if e2 := a.xm.EnsureToken(); e2 != nil {
			return nil, path, fmt.Errorf("%s 里的票据已失效（%v）", path, err)
		}
		devs, err = a.xm.Devices()
		if err != nil {
			return nil, path, fmt.Errorf("导入后仍拿不到设备：%v", err)
		}
	}
	_ = a.store.Update(func(c *Config) error {
		c.Devices = devs
		return nil
	})
	return devs, path, nil
}

func (a *App) handleImport(w http.ResponseWriter, r *http.Request) {
	devs, path, err := a.ImportXiaomusicToken()
	if err != nil {
		a.store.AddLog("系统", "", false, "导入 xiaomusic 登录态失败："+err.Error())
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	a.store.AddLog("系统", "", true, fmt.Sprintf("已从 %s 导入登录态，拿到 %d 台设备", path, len(devs)))
	writeJSON(w, 200, map[string]any{"ok": true, "path": path, "devices": devs})
}
