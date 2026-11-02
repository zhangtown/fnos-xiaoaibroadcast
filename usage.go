// 本地语音合成用量统计：只记「真的调了接口」的次数与字数（命中缓存不计），
// 落盘到数据目录 usage.json，重启不清零。
//
// 注意：这是本地估算，不是账单。精确费用要看阿里云控制台的「费用账单」。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type usageBucket struct {
	Calls int   `json:"calls"`
	Chars int   `json:"chars"`
	Ms    int64 `json:"ms"`
}

type usageStore struct {
	mu      sync.Mutex
	path    string
	Total   usageBucket             `json:"total"`
	ByModel map[string]*usageBucket `json:"byModel"`
}

func newUsageStore(dir string) *usageStore {
	u := &usageStore{path: filepath.Join(dir, "usage.json"), ByModel: map[string]*usageBucket{}}
	if data, err := os.ReadFile(u.path); err == nil {
		_ = json.Unmarshal(data, u)
		if u.ByModel == nil {
			u.ByModel = map[string]*usageBucket{}
		}
	}
	return u
}

func (u *usageStore) add(model string, chars int, ms int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.Total.Calls++
	u.Total.Chars += chars
	u.Total.Ms += ms
	b := u.ByModel[model]
	if b == nil {
		b = &usageBucket{}
		u.ByModel[model] = b
	}
	b.Calls++
	b.Chars += chars
	b.Ms += ms
	if data, err := json.MarshalIndent(u, "", "  "); err == nil {
		_ = os.WriteFile(u.path, data, 0o644)
	}
}

func (u *usageStore) snapshot() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	byModel := make([]map[string]any, 0, len(u.ByModel))
	for model, b := range u.ByModel {
		byModel = append(byModel, map[string]any{
			"model": model, "calls": b.Calls, "chars": b.Chars, "ms": b.Ms,
		})
	}
	return map[string]any{
		"total":   map[string]any{"calls": u.Total.Calls, "chars": u.Total.Chars, "ms": u.Total.Ms},
		"byModel": byModel,
	}
}
