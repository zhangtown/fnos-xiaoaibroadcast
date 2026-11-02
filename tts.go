// 语音合成：几个引擎，统一产出 mp3 字节，再经只读音源口推给音箱。
//
//	openai  —— 任何 OpenAI 兼容的 /audio/speech（硅基流动、Kokoro、Xinference、Azure 中转都行）
//	qwen    —— 阿里云百炼 Qwen-Audio-TTS（WebSocket，见 qwen.go）
//	tencent —— 腾讯云语音合成（TC3 签名，见 tencent.go）
//
// 这里只留通用的部分：缓存、投送、试听、保存，以及 Qwen 复用的那个手写 WebSocket 客户端。
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var netDialer = net.Dialer{Timeout: 15 * time.Second}

type edgeVoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// OpenAI 兼容语音接口的音色（具体有哪些取决于网关后面接的服务）。
var openaiVoices = []edgeVoice{
	{"nova", "nova · 女声 · 温和"},
	{"shimmer", "shimmer · 女声 · 轻柔"},
	{"fable", "fable · 讲故事"},
	{"alloy", "alloy · 中性"},
	{"echo", "echo · 男声"},
	{"onyx", "onyx · 男声 · 低沉"},
	{"FunAudioLLM/CosyVoice2-0.5B:claire", "claire · 硅基流动 · 温柔女声"},
	{"FunAudioLLM/CosyVoice2-0.5B:diana", "diana · 硅基流动 · 欢快女声"},
	{"FunAudioLLM/CosyVoice2-0.5B:anna", "anna · 硅基流动 · 沉稳女声"},
	{"FunAudioLLM/CosyVoice2-0.5B:alex", "alex · 硅基流动 · 沉稳男声"},
}

var ttsHTTP = &http.Client{Timeout: 240 * time.Second}

func voiceLabel(id string) string {
	for _, list := range [][]edgeVoice{openaiVoices, tencentVoices, qwenVoices} {
		for _, v := range list {
			if v.ID == id {
				return v.Label
			}
		}
	}
	return id
}

// ---------- OpenAI 兼容语音接口 ----------

// openaiSynth 走 {base}/audio/speech，默认复用大模型那套地址和 Key。
func openaiSynth(cfg Config, text, voice string) ([]byte, error) {
	base := strings.TrimSuffix(strings.TrimSpace(cfg.Speech.Base), "/")
	if base == "" {
		base = strings.TrimSuffix(strings.TrimSpace(cfg.LLM.Base), "/")
	}
	key := strings.TrimSpace(cfg.Speech.Key)
	if key == "" {
		key = strings.TrimSpace(cfg.LLM.Key)
	}
	if base == "" || key == "" {
		return nil, fmt.Errorf("还没配置语音接口的地址和 Key（设置 → 故事语音）")
	}
	if voice == "" {
		voice = "nova"
	}
	model := strings.TrimSpace(cfg.Speech.Model)
	if model == "" {
		model = "tts-1"
	}

	// 云端 TTS 有的很慢（一个字半秒），几百字必须拆开送，否则会撞超时；
	// mp3 分片直接拼接，同编码参数播放器可以连续播。
	var audio bytes.Buffer
	for _, piece := range splitForTTS(text, 110) {
		payload, _ := json.Marshal(map[string]any{
			"model": model, "input": piece, "voice": voice, "response_format": "mp3",
		})
		req, err := http.NewRequest(http.MethodPost, base+"/audio/speech", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := ttsHTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("连不上语音接口（%s）：%v", base, err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("语音接口返回 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 240))
		}
		if len(raw) < 512 {
			return nil, fmt.Errorf("语音接口只回了 %d 字节，不像音频：%s", len(raw), truncate(string(raw), 200))
		}
		audio.Write(raw)
	}
	return audio.Bytes(), nil
}

// splitForTTS 按标点把长文本切成不超过 max 个字的片段。
func splitForTTS(text string, max int) []string {
	r := []rune(strings.TrimSpace(text))
	if len(r) <= max {
		return []string{string(r)}
	}
	const breaks = "。！？；…!?\n"
	var out []string
	for start := 0; start < len(r); {
		end := start + max
		if end >= len(r) {
			end = len(r)
		} else {
			for i := end; i > start+max/2; i-- {
				if strings.ContainsRune(breaks, r[i-1]) {
					end = i
					break
				}
			}
		}
		if piece := strings.TrimSpace(string(r[start:end])); piece != "" {
			out = append(out, piece)
		}
		start = end
	}
	return out
}

// ---------- 通用 WebSocket 收发（Qwen 用） ----------

func requestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// wsWrite 写一个客户端帧（必须掩码）。
func wsWrite(w io.Writer, opcode byte, payload []byte) error {
	var hdr []byte
	hdr = append(hdr, 0x80|opcode)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 65536:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	hdr = append(hdr, mask[:]...)
	buf := make([]byte, len(hdr)+n)
	copy(buf, hdr)
	for i := 0; i < n; i++ {
		buf[len(hdr)+i] = payload[i] ^ mask[i%4]
	}
	_, err := w.Write(buf)
	return err
}

// wsRead 读一个帧。
func wsRead(r *bufio.Reader) (byte, []byte, error) {
	h := make([]byte, 2)
	if _, err := io.ReadFull(r, h); err != nil {
		return 0, nil, err
	}
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := int(h[1] & 0x7f)
	if n == 126 {
		b := make([]byte, 2)
		if _, err := io.ReadFull(r, b); err != nil {
			return 0, nil, err
		}
		n = int(binary.BigEndian.Uint16(b))
	} else if n == 127 {
		b := make([]byte, 8)
		if _, err := io.ReadFull(r, b); err != nil {
			return 0, nil, err
		}
		n = int(binary.BigEndian.Uint64(b))
	}
	var mask []byte
	if masked {
		mask = make([]byte, 4)
		if _, err := io.ReadFull(r, mask); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

// ---------- 缓存与投送 ----------

// synthToFile 合成并缓存到数据目录，返回音箱能取到的地址。
func (a *App) synthToFile(cfg Config, text, voice string) (string, error) {
	base := cfg.Music.publicBase()
	if base == "" {
		return "", fmt.Errorf("没有可用的音源地址（端口为 0 或没探测到局域网 IP）")
	}
	sum := sha1.Sum([]byte(cfg.Speech.Engine + "\x00" + voice + "\x00" + text))
	name := hex.EncodeToString(sum[:8]) + ".mp3"
	dir := filepath.Join(dataDir(), "tts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	full := filepath.Join(dir, name)
	if st, err := os.Stat(full); err != nil || st.Size() == 0 {
		var mp3 []byte
		var err error
		started := time.Now()
		switch cfg.Speech.Engine {
		case "openai":
			mp3, err = openaiSynth(cfg, text, voice)
		case "tencent":
			mp3, err = tencentSynth(cfg, text, voice)
		default:
			mp3, err = qwenSynth(cfg, text, voice)
		}
		if err != nil {
			return "", err
		}
		// 只有真的调了接口才记账（命中缓存不重复计）
		a.usage.add(cfg.Speech.Engine+"/"+cfg.Speech.Model, len([]rune(text)), time.Since(started).Milliseconds())
		tmp := full + ".tmp"
		if err := os.WriteFile(tmp, mp3, 0o644); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, full); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%s/tts/%s/%s", base, cfg.Music.Token, name), nil
}

// ttsHandler 把 /tts/<token>/<file>.mp3 映射到数据目录的 tts/。
func (a *App) ttsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := a.store.Get()
		prefix := "/tts/" + cfg.Music.Token + "/"
		if cfg.Music.Token == "" || !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		full := filepath.Join(dataDir(), "tts", name)
		if st, err := os.Stat(full); err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, full)
	})
}

// speakStory 讲故事专用：选了合成引擎就先合成再放，失败退回小爱朗读。
func (a *App) speakStory(cfg Config, text string) error {
	if cfg.Speech.Engine != "" {
		url, err := a.synthToFile(cfg, text, cfg.Speech.Voice)
		if err == nil {
			err = a.xm.PlayURL(url, 0)
		}
		if err == nil {
			a.store.AddLog("故事", text, true, "合成播放（"+voiceLabel(cfg.Speech.Voice)+"）")
			return nil
		}
		a.store.AddLog("故事", text, false, "合成失败，退回小爱："+err.Error())
	}
	return a.speak(cfg, text)
}

// ---------- 接口 ----------

func validEngine(e string) bool {
	switch e {
	case "", "openai", "qwen", "tencent":
		return true
	}
	return false
}

func voicesFor(engine string) []edgeVoice {
	switch engine {
	case "openai":
		return openaiVoices
	case "tencent":
		return tencentVoices
	default:
		return qwenVoices
	}
}

// handleSpeechSave 保存「故事语音」设置。
func (a *App) handleSpeechSave(w http.ResponseWriter, r *http.Request) {
	var in Speech
	if !readJSON(w, r, &in) {
		return
	}
	if !validEngine(in.Engine) {
		in.Engine = ""
	}
	list := voicesFor(in.Engine)
	ok := false
	for _, v := range list {
		if v.ID == in.Voice {
			ok = true
		}
	}
	if !ok && in.Voice == "" {
		in.Voice = list[0].ID
	}
	in.Base = strings.TrimSpace(in.Base)
	in.Key = strings.TrimSpace(in.Key)
	in.Model = strings.TrimSpace(in.Model)
	_ = a.store.Update(func(c *Config) error {
		old := c.Speech
		c.Speech = in
		// 页面上的密钥框留空 = 不修改（否则每次保存都会把 key 清掉）
		if in.Key == "" {
			c.Speech.Key = old.Key
		}
		if in.SecretID == "" {
			c.Speech.SecretID = old.SecretID
		}
		if in.SecretKey == "" {
			c.Speech.SecretKey = old.SecretKey
		}
		return nil
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleSpeechTest 试听：合成一句话直接放出来（不经过大模型）。
func (a *App) handleSpeechTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Engine    string `json:"engine"`
		Voice     string `json:"voice"`
		Text      string `json:"text"`
		Base      string `json:"base"`
		Key       string `json:"key"`
		Model     string `json:"model"`
		SecretID  string `json:"secretId"`
		SecretKey string `json:"secretKey"`
		Region    string `json:"region"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	cfg := a.store.Get()
	engine := in.Engine
	if !validEngine(engine) || engine == "" {
		engine = cfg.Speech.Engine
	}
	if engine == "" {
		engine = "qwen" // 试听默认试合成引擎，省得先去改设置
	}
	voice := in.Voice
	if voice == "" {
		voice = cfg.Speech.Voice
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		text = "你好，我是" + voiceLabel(voice) + "。现在，我要给你讲一个睡前故事。"
	}

	// 试听用临时配置，不改已保存的设置
	probe := cfg
	probe.Speech.Engine = engine
	probe.Speech.Voice = voice
	if b := strings.TrimSpace(in.Base); b != "" {
		probe.Speech.Base = b
	}
	if k := strings.TrimSpace(in.Key); k != "" {
		probe.Speech.Key = k
	}
	if m := strings.TrimSpace(in.Model); m != "" {
		probe.Speech.Model = m
	}
	if s := strings.TrimSpace(in.SecretID); s != "" {
		probe.Speech.SecretID = s
	}
	if s := strings.TrimSpace(in.SecretKey); s != "" {
		probe.Speech.SecretKey = s
	}
	if s := strings.TrimSpace(in.Region); s != "" {
		probe.Speech.Region = s
	}

	started := time.Now()
	url, err := a.synthToFile(probe, text, voice)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := a.xm.PlayURL(url, 0); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "url": url})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "url": url, "ms": time.Since(started).Milliseconds(), "voice": voiceLabel(voice),
	})
}
