// 批量预生成：把内置经典故事按当前引擎/音色全部合成一遍并落缓存，
// 之后点「让小爱讲」直接读缓存，不再等合成。
package main

import (
	"net/http"
	"sync"
	"time"
)

type prewarmState struct {
	mu      sync.Mutex
	running bool
	done    int
	total   int
	current string
	err     string
	started time.Time
}

func (p *prewarmState) snapshot() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]any{
		"running": p.running, "done": p.done, "total": p.total,
		"current": p.current, "error": p.err,
	}
	if p.running {
		out["elapsed"] = int(time.Since(p.started).Seconds())
	}
	return out
}

func (p *prewarmState) set(fn func(*prewarmState)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(p)
}

// handlePrewarm 开始批量预生成（异步）。
func (a *App) handlePrewarm(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	if cfg.Speech.Engine == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "先把故事语音的合成引擎保存好，再预生成"})
		return
	}
	items := a.listClassics()
	if len(items) == 0 {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "故事集是空的"})
		return
	}
	if a.prewarm.snapshot()["running"] == true {
		writeJSON(w, 200, map[string]any{"ok": true, "note": "已经在跑了"})
		return
	}

	a.prewarm.set(func(p *prewarmState) {
		p.running, p.done, p.total, p.current, p.err = true, 0, len(items), "", ""
		p.started = time.Now()
	})

	go func() {
		defer a.prewarm.set(func(p *prewarmState) { p.running = false; p.current = "" })
		for _, it := range items {
			a.prewarm.set(func(p *prewarmState) { p.current = it.Category + "/" + it.Name })
			text, err := a.readClassic(it.Category, it.Name)
			if err != nil {
				continue
			}
			if _, err := a.synthToFile(cfg, text, cfg.Speech.Voice); err != nil {
				a.prewarm.set(func(p *prewarmState) { p.err = it.Name + "：" + err.Error() })
				// 连续失败就停，别把额度耗光
				break
			}
			a.prewarm.set(func(p *prewarmState) { p.done++ })
		}
		a.store.AddLog("故事", "", true, "预生成完成：缓存就绪，之后秒播")
	}()

	writeJSON(w, 200, map[string]any{"ok": true, "total": len(items)})
}
