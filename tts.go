// Edge TTS：直连微软 Edge「大声朗读」的免费接口，把文字合成 mp3。
//
// 为什么不用第三方库：整个工程零依赖，所以这里手写了一个够用的 WebSocket 客户端
// （握手 + 掩码帧 + 分片拼接），只实现这件事需要的部分。
//
// 合成结果按「文本+音色」的哈希缓存到数据目录 tts/ 下，同一篇故事第二次播放不再重复合成。
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
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
	edgeHost  = "speech.platform.bing.com"
	edgeToken = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	edgeUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36 Edg/124.0.0.0"
)

var netDialer = net.Dialer{Timeout: 15 * time.Second}

type edgeVoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// 先放这几个够用的：女声晓晓/晓伊、男声云希/云健，外加东北话和台湾腔各一个。
var edgeVoices = []edgeVoice{
	{"zh-CN-XiaoxiaoNeural", "晓晓 · 女声 · 标准"},
	{"zh-CN-XiaoyiNeural", "晓伊 · 女声 · 温柔"},
	{"zh-CN-YunxiNeural", "云希 · 男声 · 年轻"},
	{"zh-CN-YunjianNeural", "云健 · 男声 · 浑厚"},
	{"zh-CN-liaoning-XiaobeiNeural", "晓北 · 女声 · 东北"},
	{"zh-CN-Taiwan-XiaochenNeural", "晓辰 · 女声 · 台湾"},
}

func edgeVoiceLabel(id string) string {
	for _, v := range edgeVoices {
		if v.ID == id {
			return v.Label
		}
	}
	return id
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

	// ---- 握手 ----
	var rnd [16]byte
	_, _ = rand.Read(rnd[:])
	key := base64.StdEncoding.EncodeToString(rnd[:])
	path := "/consumer/speech/synthesize/readaloud/edge/v1?TrustedClientToken=" + edgeToken
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + edgeHost + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Origin: https://edge.microsoft.com\r\n" +
		"User-Agent: " + edgeUA + "\r\n" +
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
		return nil, fmt.Errorf("Edge TTS 拒绝了连接（%s）", strings.TrimSpace(status))
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

	ts := time.Now().UTC()
	stamp := ts.Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)"

	// ---- 语音参数 ----
	cfgMsg := "X-Timestamp:" + stamp + "\r\n" +
		"Content-Type:application/json; charset=utf-8\r\n" +
		"Path:speech.config\r\n\r\n" +
		`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":"audio-24khz-48kbitrate-mono-mp3"}}}}`
	if err := wsWrite(conn, 0x1, []byte(cfgMsg)); err != nil {
		return nil, fmt.Errorf("发送配置帧失败：%v", err)
	}

	// ---- SSML ----
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

	// ---- 收音频 ----
	var audio bytes.Buffer
	for {
		op, payload, err := wsRead(br)
		if err != nil {
			if audio.Len() > 0 {
				break // 已经拿到音频，结尾帧丢了也认
			}
			return nil, fmt.Errorf("接收音频失败：%v", err)
		}
		switch op {
		case 0x2: // binary：前两字节是头长度
			if len(payload) < 2 {
				continue
			}
			hlen := int(binary.BigEndian.Uint16(payload[:2]))
			if 2+hlen > len(payload) {
				continue
			}
			header := string(payload[2 : 2+hlen])
			if strings.Contains(header, "Path:audio") {
				audio.Write(payload[2+hlen:])
			}
		case 0x1: // text：看是不是 turn.end
			if strings.Contains(string(payload), "Path:turn.end") {
				if audio.Len() == 0 {
					return nil, fmt.Errorf("Edge TTS 没有返回音频（文本可能被拒）")
				}
				return audio.Bytes(), nil
			}
		case 0x8: // close
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

// synthToFile 合成并缓存到数据目录，返回音箱能取到的地址。
func (a *App) synthToFile(cfg Config, text, voice string) (string, error) {
	base := cfg.Music.publicBase()
	if base == "" {
		return "", fmt.Errorf("没有可用的音源地址（端口为 0 或没探测到局域网 IP）")
	}
	sum := sha1.Sum([]byte(voice + "\x00" + text))
	name := hex.EncodeToString(sum[:8]) + ".mp3"
	dir := filepath.Join(dataDir(), "tts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	full := filepath.Join(dir, name)
	if st, err := os.Stat(full); err != nil || st.Size() == 0 {
		mp3, err := edgeSynth(text, voice)
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

// speakStory 讲故事专用：开了 Edge 就先合成再放，失败退回小爱朗读。
func (a *App) speakStory(cfg Config, text string) error {
	if cfg.Speech.Engine == "edge" {
		url, err := a.synthToFile(cfg, text, cfg.Speech.Voice)
		if err == nil {
			err = a.xm.PlayURL(url, 0)
		}
		if err == nil {
			a.store.AddLog("故事", text, true, "Edge 合成播放（"+edgeVoiceLabel(cfg.Speech.Voice)+"）")
			return nil
		}
		a.store.AddLog("故事", text, false, "Edge 失败，退回小爱："+err.Error())
	}
	return a.speak(cfg, text)
}

// handleSpeechSave 保存「故事语音」设置。
func (a *App) handleSpeechSave(w http.ResponseWriter, r *http.Request) {
	var in Speech
	if !readJSON(w, r, &in) {
		return
	}
	if in.Engine != "edge" {
		in.Engine = ""
	}
	ok := false
	for _, v := range edgeVoices {
		if v.ID == in.Voice {
			ok = true
		}
	}
	if !ok {
		in.Voice = edgeVoices[0].ID
	}
	_ = a.store.Update(func(c *Config) error {
		c.Speech = in
		return nil
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleSpeechTest 试听：合成一句话直接放出来（不经过大模型）。
func (a *App) handleSpeechTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Voice string `json:"voice"`
		Text  string `json:"text"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	cfg := a.store.Get()
	voice := in.Voice
	if voice == "" {
		voice = cfg.Speech.Voice
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		text = "你好，我是" + edgeVoiceLabel(voice) + "。现在，我要给你讲一个故事。"
	}
	started := time.Now()
	url, err := a.synthToFile(cfg, text, voice)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := a.xm.PlayURL(url, 0); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "url": url})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "url": url, "ms": time.Since(started).Milliseconds(),
		"voice": edgeVoiceLabel(voice),
	})
}
