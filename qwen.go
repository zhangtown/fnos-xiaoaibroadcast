// 阿里云百炼 Qwen-Audio-TTS：WebSocket 协议，实时合成。
//
// 流程（官方文档的 protocol）：连上 wss → 发 run-task → 等 task-started →
// 发若干 continue-task（带文本）→ 发 finish-task → 收 binary 帧（音频）→ 收 task-finished。
//
// 复用 tts.go 里那个手写的 WebSocket 客户端（wsWrite / wsRead），不引第三方库。
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	qwenDefaultURL   = "wss://dashscope.aliyuncs.com/api-ws/v1/inference"
	qwenDefaultModel = "qwen-audio-3.1-tts-flash"
	qwenDefaultVoice = "longanhuan_v3.1"
)

// 百炼 Qwen-Audio-TTS 的模型（北京/新加坡都能用的四个）。
var qwenModels = []string{
	"qwen-audio-3.1-tts-flash",
	"qwen-audio-3.1-tts-plus",
	"qwen-audio-3.0-tts-flash",
	"qwen-audio-3.0-tts-plus",
}

// 音色是跟模型绑定的（文档：不能跨模型混用，混了会报 InvalidParameter）。
// 这里按模型分开列，换模型时前端会跟着换列表。
var qwenVoices31Flash = []edgeVoice{
	{"wenhuaiqing_v3.1", "温怀清 · 女声 · 清亮柔和（儿童故事）"},
	{"qiaoxiaojiao_v3.1", "乔小娇 · 女声 · 俏丽可爱"},
	{"xiaxiaochen_v3.1", "夏小晨 · 女声 · 元气明亮"},
	{"yuxiaoyun_v3.1", "于小云 · 女声 · 元气亲切"},
	{"longanhuan_v3.1", "龙安欢 · 女声 · 活力"},
	{"longanlingxin_v3.1", "龙安灵心 · 女声 · 温柔守候"},
	{"longanfengyue_v3.1", "龙安风悦 · 女声 · 自然"},
	{"anruorou_v3.1", "安若柔 · 女声 · 气声知性"},
	{"anxiaolan_v3.1", "安小岚 · 女声 · 清甜纯净"},
	{"xieshurou_v3.1", "谢舒柔 · 女声 · 柔和知性"},
	{"longpaopao_v3.1", "龙泡泡 · 女声 · 童趣"},
	{"longniuniu_v3.1", "龙牛牛 · 男童声 · 儿童有声书"},
	{"longjielidou_v3.1", "龙杰力豆 · 男童声 · 天真"},
	{"longke_v3.1", "龙可 · 儿童 · 懵懂"},
	{"anmingyuan_v3.1", "安明远 · 男声 · 清亮自然"},
	{"xunanchuan_v3.1", "许南川 · 男声 · 磁性"},
	{"longshuo_v3.1", "龙硕 · 男声 · 干练"},
	{"loongbella_v3.1", "Bella · 女声 · 中英双语"},
}

// 3.1-plus 文档里只列了四个“旗舰音色”
var qwenVoices31Plus = []edgeVoice{
	{"longanlingxin_v3.1", "龙安灵心 · 女声 · 旗舰"},
	{"longanhuan_v3.1", "龙安欢 · 女声 · 旗舰"},
	{"longanfengyue_v3.1", "龙安风悦 · 女声 · 旗舰"},
	{"xunanchuan_v3.1", "许南川 · 男声 · 旗舰"},
}

// 3.0 系列文档没单独给表，先沿用同档音色；不对的话服务端会明确报错
var qwenVoices30Flash = qwenVoices31Flash
var qwenVoices30Plus = qwenVoices31Plus

// qwenVoicesByModel 按模型给音色；自定义模型回落到 flash 那一份。
var qwenVoicesByModel = map[string][]edgeVoice{
	"qwen-audio-3.1-tts-flash": qwenVoices31Flash,
	"qwen-audio-3.1-tts-plus":  qwenVoices31Plus,
	"qwen-audio-3.0-tts-flash": qwenVoices30Flash,
	"qwen-audio-3.0-tts-plus":  qwenVoices30Plus,
}

// qwenVoices 是所有候选的并集，仅用于校验/兜底。
var qwenVoices = append(append([]edgeVoice{}, qwenVoices31Flash...), qwenVoices31Plus...)

func qwenVoicesFor(model string) []edgeVoice {
	if v, ok := qwenVoicesByModel[model]; ok {
		return v
	}
	return qwenVoices31Flash
}
type wsEvent struct {
	Header struct {
		Event        string `json:"event"`
		TaskID       string `json:"task_id"`
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_message"`
	} `json:"header"`
	Payload struct {
		Output struct {
			Audio string `json:"audio"`
		} `json:"output"`
	} `json:"payload"`
}

// qwenSynth 合成一段语音，返回 mp3 字节。
func qwenSynth(cfg Config, text, voice string) ([]byte, error) {
	key := strings.TrimSpace(cfg.Speech.Key)
	if key == "" {
		return nil, fmt.Errorf("还没填阿里云百炼的 API Key（设置 → 故事语音）")
	}
	endpoint := strings.TrimSpace(cfg.Speech.Base)
	if endpoint == "" {
		endpoint = qwenDefaultURL
	}
	endpoint = strings.Replace(endpoint, "https://", "wss://", 1)
	// 防呆：引擎切到百炼但地址还是别的家（比如硅基流动的 /v1）时，别把请求发到人家那儿去
	if !strings.Contains(endpoint, "aliyuncs.com") && !strings.Contains(endpoint, "dashscope") {
		endpoint = qwenDefaultURL
	}
	model := strings.TrimSpace(cfg.Speech.Model)
	if model == "" {
		model = qwenDefaultModel
	}
	if strings.TrimSpace(voice) == "" {
		voice = qwenDefaultVoice
	}

	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("语音接口地址不对：%s", endpoint)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}

	conn, err := tls.DialWithDialer(&netDialer, "tcp", host, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, fmt.Errorf("连不上百炼（%s）：%v", u.Hostname(), err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(150 * time.Second))

	// ---- 握手（带上鉴权头）----
	var rnd [16]byte
	_, _ = rand.Read(rnd[:])
	path := u.RequestURI()
	if path == "" {
		path = "/api-ws/v1/inference"
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(rnd[:]) + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Authorization: bearer " + key + "\r\n" +
		"X-DashScope-DataInspection: enable\r\n" +
		"User-Agent: xiaoai-broadcast/1.0\r\n\r\n"
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
		for i := 0; i < 10; i++ {
			line, err := br.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
			if strings.Contains(strings.ToLower(line), "www-authenticate") || strings.Contains(strings.ToLower(line), "x-dashscope") {
				detail += strings.TrimSpace(line) + " "
			}
		}
		return nil, fmt.Errorf("百炼拒绝了连接（%s）%s 地址=%s", strings.TrimSpace(status), detail, endpoint)
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

	taskID := requestID()

	// ---- run-task ----
	runTask := map[string]any{
		"header": map[string]any{"action": "run-task", "task_id": taskID, "streaming": "duplex"},
		"payload": map[string]any{
			"task_group": "audio",
			"task":       "tts",
			"function":   "SpeechSynthesizer",
			"model":      model,
			"parameters": map[string]any{
				"text_type":   "PlainText",
				"voice":       voice,
				"format":      "mp3",
				"sample_rate": 22050,
				"volume":      50,
				"rate":        1,
				"pitch":       1,
				"enable_ssml": false,
			},
			"input": map[string]any{},
		},
	}
	if err := wsWriteJSON(conn, runTask); err != nil {
		return nil, fmt.Errorf("发送 run-task 失败：%v", err)
	}

	var audio bytes.Buffer
	started := false
	pieces := splitForTTS(text, 300)
	pieceIdx := 0

	for {
		op, payload, err := wsRead(br)
		if err != nil {
			if audio.Len() > 0 {
				return audio.Bytes(), nil
			}
			return nil, fmt.Errorf("接收失败：%v", err)
		}
		switch op {
		case 0x2: // binary：音频
			audio.Write(payload)
		case 0x1: // text：事件
			var ev wsEvent
			if json.Unmarshal(payload, &ev) != nil {
				continue
			}
			switch ev.Header.Event {
			case "task-started":
				started = true
				for pieceIdx < len(pieces) {
					msg := map[string]any{
						"header":  map[string]any{"action": "continue-task", "task_id": taskID, "streaming": "duplex"},
						"payload": map[string]any{"input": map[string]any{"text": pieces[pieceIdx]}},
					}
					if err := wsWriteJSON(conn, msg); err != nil {
						return nil, fmt.Errorf("发送 continue-task 失败：%v", err)
					}
					pieceIdx++
				}
				fin := map[string]any{
					"header":  map[string]any{"action": "finish-task", "task_id": taskID, "streaming": "duplex"},
					"payload": map[string]any{"input": map[string]any{}},
				}
				if err := wsWriteJSON(conn, fin); err != nil {
					return nil, fmt.Errorf("发送 finish-task 失败：%v", err)
				}
			case "task-finished":
				if audio.Len() == 0 {
					return nil, fmt.Errorf("百炼没有返回音频")
				}
				return audio.Bytes(), nil
			case "task-failed":
				return nil, fmt.Errorf("百炼合成失败（%s）：%s", ev.Header.ErrorCode, ev.Header.ErrorMessage)
			}
		case 0x8:
			if audio.Len() > 0 {
				return audio.Bytes(), nil
			}
			if !started {
				return nil, fmt.Errorf("连接被关闭（没有收到 task-started）")
			}
			return nil, fmt.Errorf("连接被关闭，没有音频返回")
		}
	}
}

func wsWriteJSON(w interface{ Write([]byte) (int, error) }, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return wsWrite(w, 0x1, b)
}
