// 讲故事：调一个 OpenAI 兼容的 chat 接口编故事，再交给小爱朗读。
//
// 提示词来自播报表那一条的输入框（可以留空，留空就用默认的"随便讲个童话"）。
// 只输出正文——system 里已经压掉了标题/解释/markdown，方便直接念。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultStoryPrompt = "给幼儿园的小朋友讲一个温暖的小童话，主角是一个小朋友和小动物。故事要口语化、句子短，结尾用一句话点出一个小道理。"

var storyHTTP = &http.Client{Timeout: 75 * time.Second}

type storyResult struct {
	Text   string `json:"text"`
	Model  string `json:"model"`
	TookMs int64  `json:"tookMs"`
}

// GenerateStory 调大模型编一个故事。
func (a *App) GenerateStory(prompt string) (storyResult, error) {
	cfg := a.store.Get()
	if strings.TrimSpace(cfg.LLM.Key) == "" {
		return storyResult{}, fmt.Errorf("还没配置大模型：到「故事」卡片里填 API Key（兼容 OpenAI 的接口）")
	}
	base := strings.TrimSuffix(strings.TrimSpace(cfg.LLM.Base), "/")
	if base == "" {
		base = "https://api.deepseek.com/v1"
	}
	model := strings.TrimSpace(cfg.LLM.Model)
	if model == "" {
		model = "deepseek-chat"
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = defaultStoryPrompt
	}

	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "你是给小朋友讲故事的作者。只输出故事正文：不要标题、不要分段编号、不要 markdown、不要解说。"},
			{"role": "user", "content": prompt + "\n\n朗读要求：250-350 字，句子短，口语化，适合小爱音箱直接念出来。"},
		},
		"temperature": 1.1,
		"stream":      false,
	}
	if cfg.LLM.MaxTokens > 0 {
		payload["max_tokens"] = cfg.LLM.MaxTokens
	}
	if e := strings.TrimSpace(cfg.LLM.Effort); e != "" && e != "default" {
		payload["reasoning_effort"] = e
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return storyResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLM.Key)

	started := time.Now()
	resp, err := storyHTTP.Do(req)
	if err != nil {
		return storyResult{}, fmt.Errorf("连不上大模型（%s）：%v", base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return storyResult{}, fmt.Errorf("大模型返回 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 240))
	}

	var data struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return storyResult{}, fmt.Errorf("大模型响应解析失败：%s", truncate(string(raw), 240))
	}
	if len(data.Choices) == 0 {
		return storyResult{}, fmt.Errorf("大模型没给内容：%s", truncate(string(raw), 240))
	}
	rawText := data.Choices[0].Message.Content
	text := cleanStory(rawText)
	if text == "" {
		if strings.TrimSpace(rawText) == "" {
			return storyResult{}, fmt.Errorf("模型只回了思考、没有正文：多半是推理模型且 max_tokens（%d）被思考吃光了，换 deepseek-chat 这类非推理模型，或把上限调大", cfg.LLM.MaxTokens)
		}
		return storyResult{}, fmt.Errorf("大模型返回了空故事")
	}
	return storyResult{Text: text, Model: model, TookMs: time.Since(started).Milliseconds()}, nil
}

// cleanStory 去掉 markdown/多余空行，并裁到朗读友好长度。
func cleanStory(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "**", "")
	s = strings.TrimSpace(s)
	// 去掉开头的「标题：」式行
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		out = append(out, ln)
	}
	s = strings.Join(out, "\n")
	// 小爱朗读别太长，600 字封顶，尽量在句号处断开
	r := []rune(s)
	if len(r) > 600 {
		cut := 600
		for i := 599; i > 300; i-- {
			if r[i] == '。' || r[i] == '！' || r[i] == '？' || r[i] == '\n' {
				cut = i + 1
				break
			}
		}
		r = r[:cut]
	}
	return strings.TrimSpace(string(r))
}

// handleStoryGenerate 只生成不朗读：界面上先看稿，满意了再点「让小爱讲」。
func (a *App) handleStoryGenerate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Prompt string `json:"prompt"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	res, err := a.GenerateStory(in.Prompt)
	if err != nil {
		a.store.AddLog("故事", in.Prompt, false, "生成失败："+err.Error())
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "story": res.Text, "model": res.Model, "chars": len([]rune(res.Text)), "tookMs": res.TookMs,
	})
}

// handleStorySpeak 把已经生成好的稿子交给音箱念。
func (a *App) handleStorySpeak(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	cfg := a.store.Get()
	text := strings.TrimSpace(in.Text)
	if text == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "还没有故事内容"})
		return
	}
	if err := a.speak(cfg, text); err != nil {
		a.store.AddLog("故事", text, false, err.Error())
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	a.store.AddLog("故事", text, true, fmt.Sprintf("已讲（%d 字）", len([]rune(text))))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleLLM 保存大模型设置。
func (a *App) handleLLM(w http.ResponseWriter, r *http.Request) {
	var in LLM
	if !readJSON(w, r, &in) {
		return
	}
	in.Base = strings.TrimSpace(in.Base)
	in.Key = strings.TrimSpace(in.Key)
	in.Model = strings.TrimSpace(in.Model)
	if in.Base == "" {
		in.Base = "https://api.deepseek.com/v1"
	}
	if in.Model == "" {
		in.Model = "deepseek-chat"
	}
	if in.MaxTokens < 0 {
		in.MaxTokens = 0
	}
	if in.MaxTokens > 8000 {
		in.MaxTokens = 8000
	}
	switch in.Effort {
	case "", "none", "low", "medium", "high":
	default:
		in.Effort = ""
	}
	// key 留空表示「不修改」，避免界面里只显示掩码时把 key 冲掉
	_ = a.store.Update(func(c *Config) error {
		old := c.LLM
		c.LLM = in
		if in.Key == "" {
			c.LLM.Key = old.Key
		}
		return nil
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// maskKey 只显示尾号，避免把 key 回显到页面上。
func maskKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	r := []rune(k)
	if len(r) <= 6 {
		return "••••"
	}
	head := string(r[:3])
	tail := string(r[len(r)-4:])
	return head + "••••" + tail
}

// handleLLMTest 用 GET {base}/models 验证地址与 Key，顺便把可选模型带回来。
func (a *App) handleLLMTest(w http.ResponseWriter, r *http.Request) {
	var in LLM
	if !readJSON(w, r, &in) {
		return
	}
	cfg := a.store.Get()
	base := strings.TrimSuffix(strings.TrimSpace(in.Base), "/")
	if base == "" {
		base = strings.TrimSuffix(strings.TrimSpace(cfg.LLM.Base), "/")
	}
	if base == "" {
		base = "https://api.deepseek.com/v1"
	}
	key := strings.TrimSpace(in.Key)
	if key == "" {
		key = cfg.LLM.Key
	}
	if key == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "还没有 Key"})
		return
	}

	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := storyHTTP.Do(req)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": fmt.Sprintf("连不上 %s：%v", base, err)})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, 200, map[string]any{"ok": false, "status": resp.StatusCode, "error": truncate(string(raw), 300)})
		return
	}
	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	models := []string{}
	if json.Unmarshal(raw, &data) == nil {
		for _, m := range data.Data {
			if m.ID != "" {
				models = append(models, m.ID)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "base": base, "models": models})
}
