// 语音合成：两种引擎。
//
//  1. OpenAI 兼容的 {base}/audio/speech（默认走大模型那套地址和 Key）——这是主推路径；
//  2. Edge TTS：直连微软 Edge「大声朗读」的免费接口。微软在边缘按客户端特征拦非浏览器请求，
//     实测 Go / Python / 官方 edge-tts 包全是 403，留着只是备用，不通会自动退回小爱朗读。
//
// 合成结果按「引擎+音色+文本」的哈希缓存到数据目录 tts/，同一篇第二次播放不再重复合成。
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
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

const (
	edgeHost = "speech.platform.bing.com"
	// 微软 Edge「大声朗读」用的公共客户端令牌（浏览器扩展里的常量，不是个人凭据）
	edgeToken = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	// 声明自己是哪个 Chromium 版本，必须和 Sec-MS-GEC-Version 对得上（跟官方 edge-tts 保持一致）
	edgeChromium = "143.0.3650.75"
	edgeUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"
	edgeOrigin   = "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold"
)

var netDialer = net.Dialer{Timeout: 15 * time.Second}

type edgeVoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

var edgeVoices = []edgeVoice{
	{"zh-CN-XiaoxiaoNeural", "晓晓 · 女声 · 标准"},
	{"zh-CN-XiaoyiNeural", "晓伊 · 女声 · 温柔"},
	{"zh-CN-YunxiNeural", "云希 · 男声 · 年轻"},
	{"zh-CN-YunjianNeural", "云健 · 男声 · 浑厚"},
	{"zh-CN-liaoning-XiaobeiNeural", "晓北 · 女声 · 东北"},
	{"zh-CN-Taiwan-XiaochenNeural", "晓辰 · 女声 · 台湾"},
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
	for _, v := range tencentVoices {
		if v.ID == id {
			return v.Label
		}
	}
	for _, v := range edgeVoices {
		if v.ID == id {
			return v.Label
		}
	}
	for _, v := range openaiVoices {
		if v.ID == id {
			return v.Label
		}
	}
	return id
}

// ---------- OpenAI 兼容语音接口 ----------

// openaiSynth 走 {base}/audio/speech，复用大模型那套地址和 Key。
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
	// 这类云端 TTS 一个字大约半秒，几百字的故事必须拆开送，否则会撞超时。
	// mp3 是分片拼起来的（同一编码参数，播放器可以连续播）。
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

// ---------- Edge TTS ----------

// secMSGEC 是微软 2025 年加的握手校验：把「当前时间按 5 分钟取整后的 100ns 计数 + 客户端令牌」
// 做 SHA256，再转大写十六进制。少了这个参数，握手直接 403。
func secMSGEC() string {
	const winEpoch = 11644473600 // 1601-01-01 到 1970-01-01 的秒数
	secs := time.Now().Unix() + winEpoch
	secs -= secs % 300
	ticks := secs * 10_000_000 // 转成 100ns 计数
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d%s", ticks, edgeToken)))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// muid 是微软给的匿名机器 ID，官方客户端每次握手随机生成一个带上。
func muid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

// edgeSynth 合成一段语音，返回 mp3 字节。
func edgeSynth(text, voice string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("没有要念的内容")
	}
	if voice == "" {
		voice = edgeVoices[0].ID
	}

	conn, err := tls.DialWithDialer(&netDialer, "tcp", edgeHost+":443", &tls.Config{
		ServerName: edgeHost,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return nil, fmt.Errorf("连不上 Edge TTS（%s）：%v", edgeHost, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(90 * time.Second))

	var rnd [16]byte
	_, _ = rand.Read(rnd[:])
	key := base64.StdEncoding.EncodeToString(rnd[:])
	path := "/consumer/speech/synthesize/readaloud/edge/v1?TrustedClientToken=" + edgeToken +
		"&Sec-MS-GEC=" + secMSGEC() +
		"&Sec-MS-GEC-Version=1-" + edgeChromium +
		"&ConnectionId=" + strings.ReplaceAll(requestID(), "-", "")
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + edgeHost + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Origin: " + edgeOrigin + "\r\n" +
		"User-Agent: " + edgeUA + "\r\n" +
		"Accept-Encoding: gzip, deflate, br, zstd\r\n" +
		"Accept-Language: en-US,en;q=0.9\r\n" +
		"Cookie: muid=" + muid() + ";\r\n" +
		"Pragma: no-cache\r\n" +
		"Cache-Control: no-cache\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return nil, fmt.Errorf("握手发送失败：%v", err)
	}
	br := bufio.NewReaderSize(conn, 64*1024)
	status, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("握手无响应：%v", err)
	}
	if !strings.Contains(status, "101") {
		detail := ""
		for i := 0; i < 20; i++ {
			line, err := br.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
			low := strings.ToLower(line)
			if strings.Contains(low, "x-cache") || strings.Contains(low, "accept-ch") {
				detail += strings.TrimSpace(line) + " "
			}
		}
		return nil, fmt.Errorf("Edge TTS 拒绝了连接（%s）%s", strings.TrimSpace(status), detail)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("握手响应不完整：%v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	stamp := time.Now().UTC().Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)"
	cfgMsg := "X-Timestamp:" + stamp + "\r\n" +
		"Content-Type:application/json; charset=utf-8\r\n" +
		"Path:speech.config\r\n\r\n" +
		`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":"audio-24khz-48kbitrate-mono-mp3"}}}}`
	if err := wsWrite(conn, 0x1, []byte(cfgMsg)); err != nil {
		return nil, fmt.Errorf("发送配置帧失败：%v", err)
	}

	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	ssml := "<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='zh-CN'>" +
		"<voice name='" + voice + "'><prosody pitch='+0Hz' rate='-5%' volume='+0%'>" +
		esc.Replace(text) +
		"</prosody></voice></speak>"
	ssmlMsg := "X-RequestId:" + requestID() + "\r\n" +
		"Content-Type:application/ssml+xml\r\n" +
		"X-Timestamp:" + stamp + "Z\r\n" +
		"Path:ssml\r\n\r\n" + ssml
	if err := wsWrite(conn, 0x1, []byte(ssmlMsg)); err != nil {
		return nil, fmt.Errorf("发送 SSML 失败：%v", err)
	}

	var audio bytes.Buffer
	for {
		op, payload, err := wsRead(br)
		if err != nil {
			if audio.Len() > 0 {
				break
			}
			return nil, fmt.Errorf("接收音频失败：%v", err)
		}
		switch op {
		case 0x2:
			if len(payload) < 2 {
				continue
			}
			hlen := int(binary.BigEndian.Uint16(payload[:2]))
			if 2+hlen > len(payload) {
				continue
			}
			if strings.Contains(string(payload[2:2+hlen]), "Path:audio") {
				audio.Write(payload[2+hlen:])
			}
		case 0x1:
			if strings.Contains(string(payload), "Path:turn.end") {
				if audio.Len() == 0 {
					return nil, fmt.Errorf("Edge TTS 没有返回音频")
				}
				return audio.Bytes(), nil
			}
		case 0x8:
			if audio.Len() > 0 {
				return audio.Bytes(), nil
			}
			return nil, fmt.Errorf("Edge TTS 提前关闭了连接")
		}
	}
	if audio.Len() == 0 {
		return nil, fmt.Errorf("Edge TTS 没给音频")
	}
	return audio.Bytes(), nil
}

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
		switch cfg.Speech.Engine {
		case "openai":
			mp3, err = openaiSynth(cfg, text, voice)
		case "tencent":
			mp3, err = tencentSynth(cfg, text, voice)
		default:
			mp3, err = edgeSynth(text, voice)
		}
		if err != nil {
			return "", err
		}
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

// speakStory 讲故事专用：开了合成引擎就先合成再放，失败退回小爱朗读。
func (a *App) speakStory(cfg Config, text string) error {
	if cfg.Speech.Engine == "openai" || cfg.Speech.Engine == "edge" || cfg.Speech.Engine == "tencent" {
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

// handleSpeechSave 保存「故事语音」设置。
func (a *App) handleSpeechSave(w http.ResponseWriter, r *http.Request) {
	var in Speech
	if !readJSON(w, r, &in) {
		return
	}
	if in.Engine != "edge" && in.Engine != "openai" && in.Engine != "tencent" {
		in.Engine = ""
	}
	list := edgeVoices
	switch in.Engine {
	case "openai":
		list = openaiVoices
	case "tencent":
		list = tencentVoices
	}
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
		Engine string `json:"engine"`
		Voice  string `json:"voice"`
		Text   string `json:"text"`
		Base   string `json:"base"`
		Key    string `json:"key"`
		Model  string `json:"model"`
		SecretID  string `json:"secretId"`
		SecretKey string `json:"secretKey"`
		Region    string `json:"region"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	cfg := a.store.Get()
	engine := in.Engine
	if engine != "openai" && engine != "edge" && engine != "tencent" {
		engine = cfg.Speech.Engine
	}
	if engine == "" {
		engine = "openai" // 试听默认试新引擎，省得先去改设置
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
