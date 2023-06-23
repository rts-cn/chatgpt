package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	log "github.com/sirupsen/logrus"

	"git.xswitch.cn/xswitch/proto/go/proto/xctrl"
	"git.xswitch.cn/xswitch/xctrl/ctrl"
	"git.xswitch.cn/xswitch/xctrl/ctrl/nats"
	openai "github.com/sashabaranov/go-openai"
)

const ASR_ENGINE = "ali"
const TTS_ENGINE = "ali"
const TTS_VOICE = "aixia"
const traceNATS = false

var gptToken = ""
var natsURL = ""
var natsSubject = ""

func pretty(frame *runtime.Frame) (function string, file string) {
	fileName := path.Base(frame.File)
	return "", fmt.Sprintf("%s:%d", fileName, frame.Line)
}

func init() {
	gptToken = os.Getenv("GPT_TOKEN")
	natsURL = os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://127.0.0.1:4222"
	}
	natsSubject = os.Getenv("XCTRL_natsSubject")
	if natsSubject == "" {
		natsSubject = "cn.xswitch.ctrl"
	}
	log.SetReportCaller(true)
	log.SetLevel(log.DebugLevel)
	log.SetFormatter(&log.TextFormatter{
		// DisableColors: true,
		FullTimestamp:    true,
		TimestampFormat:  "2006-01-02 15:04:05.000",
		CallerPrettyfier: pretty,
	})
}

func main() {
	shutdown := make(chan os.Signal, 1)
	err := ctrl.Init(traceNATS, natsURL)
	if err != nil {
		log.Panic("ctrl init err:", err)
	}

	myNatsSubject := "cn.xswitch.ctrl"

	w := log.WithFields(log.Fields{}).Writer()
	defer w.Close()

	log.WithFields(log.Fields{
		"natsSubject": natsSubject,
	}).Info("subscribe to:")
	color.New(color.FgGreen).Fprintln(w, "小樱桃XSwitch ChatGPT Demo Started")
	ctrl.EnableApp(new(GPTHandler), myNatsSubject, "q")

	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGILL, syscall.SIGTSTP)

	<-shutdown
	log.Info("shutting down")
	os.Exit(0)
}

type GPTHandler struct {
}
type CChannel struct {
	*ctrl.Channel
	prompts []openai.ChatCompletionMessage
}

func (h *GPTHandler) ChannelEvent(ctx context.Context, c *ctrl.Channel) {
	channel := &CChannel{
		Channel: c,
		prompts: []openai.ChatCompletionMessage{},
	}
	log.WithFields(log.Fields{
		"state": channel.State,
		"uuid":  channel.Uuid,
	}).Info("Event.Channel:")
	switch channel.State {
	case "START":
		go h.handle(channel)
	case "DESTROY":
		log.WithFields(log.Fields{
			"uuid":     channel.Uuid,
			"duration": channel.Duration,
			"billsec":  channel.Billsec,
			"cause":    channel.Cause,
		}).Info("Channel Hangup")
	}
}

func (h *GPTHandler) Event(message *ctrl.Message, natsEvent nats.Event) {
	switch message.Method {
	case "Event.DetectedData":
		{
			event := new(xctrl.DetectedData)
			json.Unmarshal(*message.Params, event)
			log.WithFields(log.Fields{
				"type":     event.Type,
				"is_final": event.IsFinal,
				"text":     event.Text,
			}).Info("DetectedData:")
		}
	default:
		log.WithFields(log.Fields{
			"method": message.Method,
		}).Info("Event:")
	}
}

// quick and easy segment implementaion, split by seperators/puctations
// returns:
// bool, found one of the sep
// []string, the splited array
func segment(s string, seps string) (bool, []string) {
	if strings.Contains(s, "\n") {
		return true, strings.SplitN(s, "\n", 2)
	}
	for _, char := range seps {
		if !strings.Contains(s, string(char)) {
			continue
		}
		arr := strings.SplitAfterN(s, string(char), 2)
		return true, arr
	}
	return false, []string{s}
}

func (h *GPTHandler) handle(channel *CChannel) {
	log.WithFields(log.Fields{
		"uuid": channel.Uuid,
		"from": channel.CidNumber,
		"to":   channel.DestNumber,
	}).Info("Handle Call")
	res := channel.Answer0()
	if res.Code != 200 {
		return
	}
	prompt := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: "你是一个AI客服助理，你的名字叫小樱桃。",
	}
	channel.prompts = append(channel.prompts, prompt)
	prompt = openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: "您好，请问有什么可以帮您？",
	}
	channel.prompts = append(channel.prompts, prompt)
	time.Sleep(500 * time.Millisecond) // waiting for media
	log.Info(prompt.Content)

	TTS(channel, prompt.Content, 5*time.Second)

	for {
		beep := "[BEEP]"
		// beep = ""
		response := Detect(channel, beep, 16*time.Second)
		if response.Code != 200 {
			return
		}
		heard := response.Data.Text
		log.WithFields(log.Fields{
			"heard": heard,
		}).Info("Heard")
		if heard == "" {
			heard = "您好"
		} else if heard == "再见" || heard == "拜拜" {
			TTS(channel, "再见", 5*time.Second)
			time.Sleep(500 * time.Millisecond)
			channel.Hangup0("NORMAL_CLEARING", xctrl.HangupRequest_SELF)
			return
		}
		h.request_and_play(channel, heard)
	}
}

func (h *GPTHandler) request_and_play(channel *CChannel, heard string) {
	config := openai.DefaultConfig("dummy")
	config.BaseURL = "http://localhost:8081/api/hello/cn"
	c := openai.NewClientWithConfig(config)
	if gptToken != "" {
		c = openai.NewClient(gptToken)
	}
	channel.prompts = append(channel.prompts, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: heard,
	})
	req := openai.ChatCompletionRequest{
		Model:     openai.GPT3Dot5Turbo,
		MaxTokens: 100,
		Messages:  channel.prompts,
		Stream:    true,
	}
	ctx := context.Background()
	stream, err := c.CreateChatCompletionStream(ctx, req)
	if err != nil {
		log.Errorf("ChatCompletionStream error: %v\n", err)
		return
	}
	defer stream.Close()
	var xresponse *xctrl.Response
	text := ""
	full_text := ""
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			log.Info("Stream finished")
			if text != "" {
				TTS(channel, text, 30*time.Second)
			}
			channel.prompts = append(channel.prompts, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: full_text,
			})
			if len(channel.prompts) > 9 {
				first := channel.prompts[0:1]
				last := channel.prompts[len(channel.prompts)-9:]
				channel.prompts = append(first, last...)
			}
			return
		}
		if err != nil {
			log.Errorf("\nStream error: %v\n", err)
			return
		}
		full_text += response.Choices[0].Delta.Content
		text = text + response.Choices[0].Delta.Content
		text = strings.Trim(text, "\n")
		log.Debugf(">>> %s\n", text)

		seperators := "，。、！？；,.!?;"
		ok, arr := segment(text, seperators)
		if ok {
			w := log.WithFields(log.Fields{}).Writer()
			color.New(color.FgGreen).Fprint(w, ">>>>>> [", arr[0], "]")
			w.Close()

			xresponse = TTS(channel, arr[0], 30*time.Second)
			if xresponse != nil && xresponse.Code != 200 {
				log.WithFields(log.Fields{
					"code":     xresponse.Code,
					"message:": xresponse.Message,
				}).Warning("Response")
				break
			}
			if len(arr) == 2 {
				text = arr[1]
			} else {
				text = ""
			}
		} else {
			if len(arr) == 1 {
				text = arr[0]
			}
		}
	}

	if xresponse != nil && xresponse.Code == 200 { // the call still alive
		channel.Hangup0("NORMAL_CLEARING", xctrl.HangupRequest_SELF)
	}
}

func TTS(channel *CChannel, text string, timeout time.Duration) *xctrl.Response {
	return channel.PlayTTS(TTS_ENGINE, TTS_VOICE, text, ctrl.WithTimeout(timeout))
}

func Detect(channel *CChannel, text string, timeout time.Duration) *xctrl.DetectResponse {
	file := "silence_stream://1000"
	if text == "[BEEP]" {
		file = "tone_stream://%(80,20,640)"
		text = ""
	}
	media := &xctrl.Media{
		Type: "FILE",
		Data: file,
	}
	if text != "" {
		media.Type = "TEXT"
		media.Data = text
		media.Engine = TTS_ENGINE
		media.Voice = TTS_VOICE
	}
	req := &xctrl.DetectRequest{
		CtrlUuid: ctrl.UUID(),
		Uuid:     channel.Uuid,
		Media:    media,
		Speech: &xctrl.SpeechRequest{
			Engine:         ASR_ENGINE,
			NoInputTimeout: 5 * 1000,
			SpeechTimeout:  15 * 1000,
			PartialEvents:  true,
		},
		Dtmf: &xctrl.DTMFRequest{
			DigitTimeout: 3 * 1000,
		},
	}
	return channel.DetectSpeech(req)
}
